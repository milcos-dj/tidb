// Copyright 2021 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"bytes"
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"path"
	"runtime"
	"strings"
	"sync"
	"time"

	// Set the correct when it runs inside docker.
	_ "go.uber.org/automaxprocs"
)

func usage() {
	msg := `// run all tests
ut

// show usage
ut -h

// list all packages
ut list

// list test cases of a single package
ut list $package

// run all tests
ut run

// run test all cases of a single package
ut run $package

// run test cases of a single package
ut run $package $test

// build all test package
ut build

// build a test package
ut build xxx`
	fmt.Println(msg)
}

const modulePath = "github.com/pingcap/tidb"

type task struct {
	pkg  string
	test string
	old  bool
}

var P int
var workDir string

func cmdList(args ...string) {
	pkgs, err := listPackages()
	if err != nil {
		fmt.Println("list package error", err)
		return
	}

	// list all packages
	if len(args) == 0 {
		for _, pkg := range pkgs {
			fmt.Println(pkg)
		}
		return
	}

	// list test case of a single package
	if len(args) == 1 {
		pkg := args[0]
		pkgs = filter(pkgs, func(s string) bool { return s == pkg })
		if len(pkgs) != 1 {
			fmt.Println("package not exist", pkg)
			return
		}

		err := buildTestBinary(pkg)
		if err != nil {
			fmt.Println("build package error", pkg, err)
			return
		}
		exist, err := testBinaryExist(pkg)
		if err != nil {
			fmt.Println("check test binary existance error", err)
			return
		}
		if !exist {
			fmt.Println("no test case in ", pkg)
			return
		}

		res, err := listTestCases(pkg, nil)
		if err != nil {
			fmt.Println("list test cases for package error", err)
			return
		}
		for _, x := range res {
			fmt.Println(x.test)
		}
	}
}

func cmdBuild(args ...string) {
	pkgs, err := listPackages()
	if err != nil {
		fmt.Println("list package error", err)
		return
	}

	// build all packages
	if len(args) == 0 {
		for _, pkg := range pkgs {
			err := buildTestBinary(pkg)
			if err != nil {
				fmt.Println("build package error", pkg, err)
				return
			}
		}
		return
	}

	// build test binary of a single package
	if len(args) >= 1 {
		pkg := args[0]
		err := buildTestBinary(pkg)
		if err != nil {
			fmt.Println("build package error", pkg, err)
			return
		}
	}
}

func cmdRun(args ...string) {
	var err error
	pkgs, err := listPackages()
	if err != nil {
		fmt.Println("list packages error", err)
		return
	}
	tasks := make([]task, 0, 5000)
	// run all tests
	if len(args) == 0 {
		for _, pkg := range pkgs {
			fmt.Println("handling package", pkg)
			err := buildTestBinary(pkg)
			if err != nil {
				fmt.Println("build package error", pkg, err)
				return
			}

			exist, err := testBinaryExist(pkg)
			if err != nil {
				fmt.Println("check test binary existance error", err)
				return
			}
			if !exist {
				fmt.Println("no test case in ", pkg)
				continue
			}

			tasks, err = listTestCases(pkg, tasks)
			if err != nil {
				fmt.Println("list test cases error", err)
				return
			}
		}
	}

	// run tests for a single package
	if len(args) == 1 {
		pkg := args[0]
		err := buildTestBinary(pkg)
		if err != nil {
			fmt.Println("build package error", pkg, err)
			return
		}
		exist, err := testBinaryExist(pkg)
		if err != nil {
			fmt.Println("check test binary existance error", err)
			return
		}

		if !exist {
			fmt.Println("no test case in ", pkg)
			return
		}
		tasks, err = listTestCases(pkg, tasks)
		if err != nil {
			fmt.Println("list test cases error", err)
			return
		}
	}

	// run a single test
	if len(args) == 2 {
		pkg := args[0]
		err := buildTestBinary(pkg)
		if err != nil {
			fmt.Println("build package error", pkg, err)
			return
		}
		exist, err := testBinaryExist(pkg)
		if err != nil {
			fmt.Println("check test binary existance error", err)
			return
		}
		if !exist {
			fmt.Println("no test case in ", pkg)
			return
		}

		tasks, err = listTestCases(pkg, tasks)
		if err != nil {
			fmt.Println("list test cases error", err)
			return
		}
		// filter the test case to run
		tmp := tasks[:0]
		for _, task := range tasks {
			if strings.Contains(task.test, args[1]) {
				tmp = append(tmp, task)
			}
		}
		tasks = tmp
	}
	fmt.Println("building task finish...", len(tasks))

	numactl := numactlExist()
	taskCh := make(chan task, 100)
	var wg sync.WaitGroup
	for i := 0; i < P; i++ {
		n := numa{fmt.Sprintf("%d", i), numactl}
		wg.Add(1)
		go n.worker(&wg, taskCh)
	}

	shuffle(tasks)
	for _, task := range tasks {
		taskCh <- task
	}
	close(taskCh)
	wg.Wait()
}

func main() {
	// Get the correct count of CPU if it's in docker.
	P = runtime.GOMAXPROCS(0)
	rand.Seed(time.Now().Unix())
	var err error
	workDir, err = os.Getwd()
	if err != nil {
		fmt.Println("os.Getwd() error", err)
	}

	if len(os.Args) == 1 {
		// run all tests
		cmdRun()
		return
	}

	if len(os.Args) >= 2 {
		switch os.Args[1] {
		case "list":
			cmdList(os.Args[2:]...)
		case "build":
			cmdBuild(os.Args[2:]...)
		case "run":
			cmdRun(os.Args[2:]...)
		default:
			usage()
		}
	}
}

func listTestCases(pkg string, tasks []task) ([]task, error) {
	newCases, err := listNewTestCases(pkg)
	if err != nil {
		fmt.Println("list test case error", pkg, err)
		return nil, withTrace(err)
	}
	for _, c := range newCases {
		tasks = append(tasks, task{pkg, c, false})
	}

	oldCases, err := listOldTestCases(pkg)
	if err != nil {
		fmt.Println("list old test case error", pkg, err)
		return nil, withTrace(err)
	}
	for _, c := range oldCases {
		tasks = append(tasks, task{pkg, c, true})
	}
	return tasks, nil
}

func listPackages() ([]string, error) {
	cmd := exec.Command("go", "list", "./...")
	ss, err := cmdToLines(cmd)
	if err != nil {
		return nil, withTrace(err)
	}

	ret := ss[:0]
	for _, s := range ss {
		if !strings.HasPrefix(s, modulePath) {
			continue
		}
		pkg := s[len(modulePath)+1:]
		if skipDIR(pkg) {
			continue
		}
		ret = append(ret, pkg)
	}
	return ret, nil
}

type numa struct {
	cpu     string
	numactl bool
}

func (n *numa) worker(wg *sync.WaitGroup, ch chan task) {
	defer wg.Done()
	for t := range ch {
		start := time.Now()
		if err := n.runTestCase(t.pkg, t.test, t.old); err != nil {
			fmt.Println("run test case error", t.pkg, t.test, t.old, time.Since(start), err)
		}
	}
}

func (n *numa) runTestCase(pkg string, fn string, old bool) error {
	exe := "./" + testFileName(pkg)
	var cmd *exec.Cmd
	if n.numactl {
		cmd = n.testCommandWithNumaCtl(exe, fn, old)
	} else {
		cmd = n.testCommand(exe, fn, old)
	}
	cmd.Dir = path.Join(workDir, pkg)
	_, err := cmd.CombinedOutput()
	if err != nil {
		// fmt.Println("run test case error", pkg, fn, string(output))
		return err
	}
	return nil
}

func (n *numa) testCommandWithNumaCtl(exe string, fn string, old bool) *exec.Cmd {
	if old {
		// numactl --physcpubind 3 -- session.test -test.run '^TestT$' -check.f testTxnStateSerialSuite.TestTxnInfoWithPSProtoco
		return exec.Command(
			"numactl", "--physcpubind", n.cpu, "--",
			exe,
			"-test.timeout", "20s",
			"-test.cpu", "1", "-test.run", "^TestT$", "-check.f", fn)
	}

	// numactl --physcpubind 3 -- session.test -test.run TestClusteredPrefixColum
	return exec.Command(
		"numactl", "--physcpubind", n.cpu, "--",
		exe,
		"-test.timeout", "20s",
		"-test.cpu", "1", "-test.run", fn)
}

func (n *numa) testCommand(exe string, fn string, old bool) *exec.Cmd {
	if old {
		// session.test -test.run '^TestT$' -check.f testTxnStateSerialSuite.TestTxnInfoWithPSProtoco
		return exec.Command(
			exe,
			"-test.timeout", "20s",
			"-test.cpu", "1", "-test.run", "^TestT$", "-check.f", fn)
	}

	// session.test -test.run TestClusteredPrefixColum
	return exec.Command(
		exe,
		"-test.timeout", "20s",
		"-test.cpu", "1", "-test.run", fn)
}

func skipDIR(pkg string) bool {
	skipDir := []string{"br", "cmd", "dumpling"}
	for _, ignore := range skipDir {
		if strings.HasPrefix(pkg, ignore) {
			return true
		}
	}
	return false
}

func buildTestBinary(pkg string) error {
	// go test -c
	cmd := exec.Command("go", "test", "-c", "-vet", "off", "-o", testFileName(pkg))
	cmd.Dir = path.Join(workDir, pkg)
	err := cmd.Run()
	return withTrace(err)
}

func testBinaryExist(pkg string) (bool, error) {
	_, err := os.Stat(testFileFullPath(pkg))
	if err != nil {
		if _, ok := err.(*os.PathError); ok {
			return false, nil
		}
	}
	return true, withTrace(err)
}
func numactlExist() bool {
	find, err := exec.Command("which", "numactl").Output()
	if err == nil && len(find) > 0 {
		return true
	}
	return false
}

func testFileName(pkg string) string {
	_, file := path.Split(pkg)
	return file+".test.bin"
}

func testFileFullPath(pkg string) string {
	return path.Join(workDir, pkg, testFileName(pkg))
}

func listNewTestCases(pkg string) ([]string, error) {
	exe := "./" + testFileName(pkg)

	// session.test -test.list Test
	cmd := exec.Command(exe, "-test.list", "Test")
	cmd.Dir = path.Join(workDir, pkg)
	res, err := cmdToLines(cmd)
	if err != nil {
		return nil, withTrace(err)
	}
	return filter(res, func(s string) bool {
		return strings.HasPrefix(s, "Test") && s != "TestT" && s != "TestBenchDaily"
	}), nil
}

func listOldTestCases(pkg string) (res []string, err error) {
	exe := "./" + testFileName(pkg)

	// Maybe the restructure is finish on this package.
	cmd := exec.Command(exe, "-h")
	cmd.Dir = path.Join(workDir, pkg)
	buf, err := cmd.CombinedOutput()
	if err != nil {
		err = withTrace(err)
		return
	}
	if !bytes.Contains(buf, []byte("check.list")) {
		// there is no old test case in pkg
		return
	}

	// session.test -test.run TestT -check.list Test
	cmd = exec.Command(exe, "-test.run", "^TestT$", "-check.list", "Test")
	cmd.Dir = path.Join(workDir, pkg)
	res, err = cmdToLines(cmd)
	res = filter(res, func(s string) bool { return strings.Contains(s, "Test") })
	return res, withTrace(err)
}

func cmdToLines(cmd *exec.Cmd) ([]string, error) {
	res, err := cmd.Output()
	if err != nil {
		return nil, withTrace(err)
	}
	ss := bytes.Split(res, []byte{'\n'})
	ret := make([]string, len(ss))
	for i, s := range ss {
		ret[i] = string(s)
	}
	return ret, nil
}

func filter(input []string, f func(string) bool) []string {
	ret := input[:0]
	for _, s := range input {
		if f(s) {
			ret = append(ret, s)
		}
	}
	return ret
}

func shuffle(tasks []task) {
	for i := 0; i < len(tasks); i++ {
		pos := rand.Intn(len(tasks))
		tasks[i], tasks[pos] = tasks[pos], tasks[i]
	}
}

type errWithStack struct {
	err error
	buf []byte
}

func (e *errWithStack) Error() string {
	return e.err.Error() + "\n" + string(e.buf)
}

func withTrace(err error) error {
	if err == nil {
		return err
	}
	if _, ok := err.(*errWithStack); ok {
		return err
	}
	var stack [4096]byte
	sz := runtime.Stack(stack[:], false)
	return &errWithStack{err, stack[:sz]}
}
