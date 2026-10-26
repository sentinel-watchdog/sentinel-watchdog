//go:build integration && linux

// Package supervisor_test runs a real daemon with a process service as
// root on a real Linux system (task test-vm, D-056, D-085): the identity
// and environment of the child, the parent-death signal when the daemon
// is SIGKILLed, the cleanup of what that left at the next start (D-084
// (2)), the stop of the whole group at shutdown, bounded recovery
// across a restart of the daemon (D-087), a reload that replaces one
// service and keeps the other running (D-088), and the operator's
// commands over the control socket (D-089), and the memory limit (D-090).
// The daemon is this
// test binary run again in daemon mode, so that it can be killed. The
// suite fails, not skips, without root: it is run on purpose.
package supervisor_test

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/audit"
	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/config"
	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/module"
	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/transport"
	"github.com/sentinel-watchdog/sentinel-watchdog/internal/daemon"
	"github.com/sentinel-watchdog/sentinel-watchdog/pkg/api"
)

const (
	modeEnv   = "SENTINEL_ITEST_MODE"
	configEnv = "SENTINEL_ITEST_CONFIG"
	// daemonOnly is in the daemon's environment and must not reach the
	// child (threat model T-15).
	daemonOnly = "SENTINEL_ITEST_DAEMON_ONLY"
	secret     = "itest-literal-secret"
)

func TestMain(m *testing.M) {
	switch os.Getenv(modeEnv) {
	case "":
	case "daemon":
		os.Exit(runDaemon())
	case "worker":
		runWorker()
	case "leaf":
		signal.Ignore(syscall.SIGTERM)
		sleepForever()
	case "crasher":
		fmt.Printf("run=%d\n", os.Getpid())
		os.Exit(1)
	case "hog":
		runHog()
	default:
		os.Exit(91)
	}
	if os.Geteuid() != 0 {
		fmt.Fprintln(os.Stderr, "tests/integration/supervisor needs root (run it with task test-vm)")
		os.Exit(1)
	}
	syscall.Umask(0o022)
	os.Exit(m.Run())
}

// runDaemon is sentineld: the real daemon and modules, until SIGTERM; a
// SIGHUP reloads the configuration.
func runDaemon() int {
	reg, err := daemon.Modules()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	load := func() (*config.Config, []module.Instance, error) { return daemon.Load(reg, os.Getenv(configEnv)) }
	cfg, mods, err := load()
	if err != nil {
		_ = config.WriteDiagnostics(os.Stderr, cfg, err)
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM)
	defer stop()
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	d := daemon.New(cfg, mods, daemon.Options{Logger: slog.New(slog.NewTextHandler(os.Stderr, nil)), Version: "itest",
		Modules: reg.Availability(), Commands: reg.Commands(), Load: load})
	if err := d.Start(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "start:", err)
		return 1
	}
	for ctx.Err() == nil {
		select {
		case <-hup:
			res, apiErr := d.Reload(ctx, audit.Actor{Kind: audit.ActorSignal})
			fmt.Fprintf(os.Stderr, "reload: %+v %v\n", res, apiErr)
		case <-ctx.Done():
		}
	}
	if err := d.Stop(context.Background()); err != nil {
		fmt.Fprintln(os.Stderr, "stop:", err)
		return 1
	}
	return 0
}

// runWorker is the supervised program: it leaves a TERM-ignoring leaf in
// its group, prints the leaf's pid and waits.
func runWorker() {
	self, _ := os.Executable()
	p, err := os.StartProcess(self, []string{self}, &os.ProcAttr{
		Env: []string{modeEnv + "=leaf"}, Files: []*os.File{nil, os.Stdout, os.Stderr}})
	if err != nil {
		os.Exit(90)
	}
	fmt.Printf("leaf=%d\n", p.Pid)
	sleepForever()
}

// runHog is a program over its memory limit: it leaves a TERM-ignoring
// leaf in its group, prints its own pid and the leaf's, then holds
// 256 MiB, every page written.
func runHog() {
	self, _ := os.Executable()
	p, err := os.StartProcess(self, []string{self}, &os.ProcAttr{
		Env: []string{modeEnv + "=leaf"}, Files: []*os.File{nil, os.Stdout, os.Stderr}})
	if err != nil {
		os.Exit(90)
	}
	fmt.Printf("run=%d\nleaf=%d\n", os.Getpid(), p.Pid)
	hog = make([]byte, 256<<20)
	for i := range hog {
		hog[i] = 1
	}
	sleepForever()
}

// hog keeps runHog's memory reachable.
var hog []byte

func sleepForever() {
	for {
		time.Sleep(time.Hour)
	}
}

// setup is a configuration directory, a state directory and a copy of
// the test binary that nobody may execute (test-vm's staging directory is
// private).
type setup struct {
	dir, bin, config, stateDir, output string
}

func newSetup(t *testing.T) setup {
	t.Helper()
	dir, err := os.MkdirTemp("", "sentinel-itest-sup")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	if err := os.Chmod(dir, 0o755); err != nil { // the program in it is run as nobody
		t.Fatal(err)
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(self)
	if err != nil {
		t.Fatal(err)
	}
	s := setup{dir: dir, bin: filepath.Join(dir, "bin", "worker"), config: filepath.Join(dir, "etc", "sentinel.yaml"),
		stateDir: filepath.Join(dir, "state"), output: filepath.Join(dir, "log", "worker.log")}
	for _, d := range []string{"bin", "etc/supervisor", "log"} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(s.bin, data, 0o755); err != nil {
		t.Fatal(err)
	}
	central := "version: 1\ndaemon:\n  socket: " + filepath.Join(dir, "run", "s.sock") + "\n  state_dir: " + s.stateDir +
		"\n  shutdown_timeout: 20s\nmodules:\n  supervisor:\n    enabled: true\n    processes: true\n"
	service := "version: 1\nservices:\n  - name: worker\n    command: " + s.bin + "\n    user: \"65534\"\n    group: \"65534\"\n" +
		"    working_dir: /tmp\n    environment: {" + modeEnv + ": worker, API_TOKEN: " + secret + "}\n" +
		"    output: {file: " + s.output + "}\n    stop_timeout: 2s\n    startup_grace: 1s\n"
	writeFile(t, s.config, central)
	writeFile(t, filepath.Join(dir, "etc", "supervisor", "worker.yaml"), service)
	return s
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// startDaemon runs the daemon; stderr goes to the test log.
func (s setup) startDaemon(t *testing.T) *exec.Cmd {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(self)
	cmd.Env = []string{modeEnv + "=daemon", configEnv + "=" + s.config, daemonOnly + "=leak", "PATH=/usr/bin:/bin"}
	cmd.Stderr = testWriter{t}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	return cmd
}

type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) {
	w.t.Logf("daemon: %s", strings.TrimRight(string(p), "\n"))
	return len(p), nil
}

// workerGroup waits until the state file records the worker's group and
// returns its leader's pid (other than not, when not is non-zero).
func (s setup) workerGroup(t *testing.T, not int) int {
	t.Helper()
	return s.group(t, "worker", not)
}

// group is workerGroup for the service name.
func (s setup) group(t *testing.T, name string, not int) int {
	t.Helper()
	var pid int
	eventually(t, name+"'s group in the state file", func() bool {
		raw, err := os.ReadFile(filepath.Join(s.stateDir, "supervisor", "state.json"))
		if err != nil {
			return false
		}
		var f struct {
			Data struct {
				Groups map[string]struct{ Pgid int } `json:"groups"`
			} `json:"data"`
		}
		if json.Unmarshal(raw, &f) != nil {
			return false
		}
		pid = f.Data.Groups[name].Pgid
		return pid != 0 && pid != not
	})
	return pid
}

// leaf waits for the n-th "leaf=" line of the output file.
func (s setup) leaf(t *testing.T, n int) int {
	t.Helper()
	return leafIn(t, s.output, n)
}

// leafIn waits for the n-th "leaf=" line of the output file at path.
func leafIn(t *testing.T, path string, n int) int {
	t.Helper()
	var pid int
	eventually(t, "the leaf's pid in the output file", func() bool {
		f, err := os.Open(path)
		if err != nil {
			return false
		}
		defer f.Close()
		seen := 0
		for sc := bufio.NewScanner(f); sc.Scan(); {
			if v, ok := strings.CutPrefix(sc.Text(), "leaf="); ok {
				if seen++; seen == n {
					pid, _ = strconv.Atoi(v)
				}
			}
		}
		return pid > 0
	})
	return pid
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(20 * time.Second); !cond(); time.Sleep(50 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

// alive reports whether pid runs and is not a zombie.
func alive(pid int) bool {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return false
	}
	i := strings.LastIndexByte(string(b), ')')
	return i > 0 && i+2 < len(b) && b[i+2] != 'Z'
}

func awaitGone(t *testing.T, pid int, what string) {
	t.Helper()
	eventually(t, what+" to end", func() bool { return !alive(pid) })
}

// statusField returns a field of /proc/<pid>/status ("Uid", "Groups").
func statusField(t *testing.T, pid int, name string) string {
	t.Helper()
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		t.Fatal(err)
	}
	for line := range strings.SplitSeq(string(b), "\n") {
		if v, ok := strings.CutPrefix(line, name+":"); ok {
			return strings.Join(strings.Fields(v), " ")
		}
	}
	return ""
}

// AC2, AC4, AC5, AC6: identity and environment of the child; after a
// SIGKILL of the daemon its leader dies (Pdeathsig) and the next start
// kills what it left; a SIGTERM stops the whole group.
func TestProcessServiceLifecycle(t *testing.T) {
	s := newSetup(t)
	d := s.startDaemon(t)
	worker := s.workerGroup(t, 0)
	leaf := s.leaf(t, 1)

	if got := statusField(t, worker, "Uid"); got != "65534 65534 65534 65534" {
		t.Errorf("uid = %q", got)
	}
	if got := statusField(t, worker, "Gid"); got != "65534 65534 65534 65534" {
		t.Errorf("gid = %q", got)
	}
	if got := statusField(t, worker, "Groups"); got != "" {
		t.Errorf("supplementary groups = %q, want none", got)
	}
	environ, err := os.ReadFile(fmt.Sprintf("/proc/%d/environ", worker))
	if err != nil {
		t.Fatal(err)
	}
	env := strings.Split(strings.TrimRight(string(environ), "\x00"), "\x00")
	want := []string{"API_TOKEN=" + secret, "PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin", modeEnv + "=worker"}
	if strings.Join(env, " ") != strings.Join(want, " ") {
		t.Errorf("environment = %q, want %q", env, want)
	}
	if cwd, _ := os.Readlink(fmt.Sprintf("/proc/%d/cwd", worker)); cwd != "/tmp" {
		t.Errorf("cwd = %q", cwd)
	}

	// SIGKILL of the daemon: Pdeathsig ends the leader, the leaf stays.
	_ = d.Process.Kill()
	_ = d.Wait()
	awaitGone(t, worker, "the worker (Pdeathsig)")
	time.Sleep(200 * time.Millisecond)
	if !alive(leaf) {
		t.Fatal("the leaf died with the worker: the test would prove nothing")
	}

	// The next start kills the leaf through the record, then launches anew.
	d2 := s.startDaemon(t)
	awaitGone(t, leaf, "the leaf left by the killed daemon")
	worker2 := s.workerGroup(t, worker)
	leaf2 := s.leaf(t, 2)

	// SIGTERM: the stop signal to the group, then SIGKILL after
	// stop_timeout for the leaf that ignores it.
	if err := d2.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if err := d2.Wait(); err != nil {
		t.Errorf("daemon exit: %v", err)
	}
	if alive(worker2) || alive(leaf2) {
		t.Errorf("after shutdown: worker alive %v, leaf alive %v", alive(worker2), alive(leaf2))
	}
}

// D-086: a program another user could replace is refused by validation.
func TestUntrustedProgramRefused(t *testing.T) {
	s := newSetup(t)
	if err := os.Chmod(filepath.Join(s.dir, "bin"), 0o777); err != nil {
		t.Fatal(err)
	}
	reg, err := daemon.Modules()
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = daemon.Load(reg, s.config)
	var verr *config.ValidationError
	if !errors.As(err, &verr) || !strings.Contains(err.Error(), "untrusted executable") {
		t.Fatalf("Load = %v", err)
	}
}

// runs counts the "run=" lines of the output file.
func (s setup) runs() int {
	b, err := os.ReadFile(s.output)
	if err != nil {
		return 0
	}
	return strings.Count(string(b), "run=")
}

// D-013, D-014, D-087: a program that always exits 1 is launched once and
// restarted max_attempts times with growing delays, then exhausted; a
// restart of the daemon keeps the exhaustion; the cooldown renews the
// budget.
func TestRecoveryCrashLoop(t *testing.T) {
	s := newSetup(t)
	writeFile(t, filepath.Join(s.dir, "etc", "supervisor", "worker.yaml"), "version: 1\nservices:\n  - name: worker\n"+
		"    command: "+s.bin+"\n    environment: {"+modeEnv+": crasher}\n    output: {file: "+s.output+"}\n"+
		"    startup_grace: 0s\n    recovery: {max_attempts: 3, window: 1h, delay: 200ms, max_delay: 1s, stable_after: 1s, cooldown: 1m}\n")
	d := s.startDaemon(t)
	begin := time.Now()
	eventually(t, "four launches", func() bool { return s.runs() >= 4 })
	if took := time.Since(begin); took < 200*time.Millisecond+400*time.Millisecond+800*time.Millisecond {
		t.Errorf("four launches in %s: the delays were not applied", took)
	}
	time.Sleep(3 * time.Second)
	if n := s.runs(); n != 4 {
		t.Fatalf("%d launches: the budget did not hold", n)
	}
	raw, err := os.ReadFile(filepath.Join(s.stateDir, "supervisor", "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	var f struct {
		Data struct {
			Services map[string]struct {
				Attempts    []time.Time `json:"attempts"`
				ExhaustedAt time.Time   `json:"exhausted_at"`
			} `json:"services"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	if w := f.Data.Services["worker"]; len(w.Attempts) != 3 || w.ExhaustedAt.IsZero() {
		t.Fatalf("state file: %+v", w)
	}

	// A restart of the daemon does not renew the budget.
	if err := d.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if err := d.Wait(); err != nil {
		t.Errorf("daemon exit: %v", err)
	}
	s.startDaemon(t)
	time.Sleep(3 * time.Second)
	if n := s.runs(); n != 4 {
		t.Fatalf("%d launches after the restart: the exhaustion was forgotten", n)
	}

	// The cooldown renews it.
	deadline := f.Data.Services["worker"].ExhaustedAt.Add(time.Minute + 20*time.Second)
	for s.runs() < 5 {
		if time.Now().After(deadline) {
			t.Fatal("no launch after the cooldown")
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// D-088: a reload that changes one service replaces only that one: the
// other keeps its process (same pid, same start time), and nothing of the
// replaced group is left.
func TestReloadReplacesOnlyTheChangedService(t *testing.T) {
	s := newSetup(t)
	otherOut := filepath.Join(s.dir, "log", "other.log")
	other := func(version string) string {
		return "version: 1\nservices:\n  - name: other\n    command: " + s.bin + "\n    user: \"65534\"\n    group: \"65534\"\n" +
			"    working_dir: /tmp\n    environment: {" + modeEnv + ": worker, VERSION: \"" + version + "\"}\n" +
			"    output: {file: " + otherOut + "}\n    stop_timeout: 2s\n    startup_grace: 1s\n"
	}
	otherFile := filepath.Join(s.dir, "etc", "supervisor", "other.yaml")
	writeFile(t, otherFile, other("1"))
	d := s.startDaemon(t)
	worker, otherV1 := s.workerGroup(t, 0), s.group(t, "other", 0)
	workerStart, otherLeaf := statField(t, worker, 22), leafIn(t, otherOut, 1)

	writeFile(t, otherFile, other("2"))
	if err := d.Process.Signal(syscall.SIGHUP); err != nil {
		t.Fatal(err)
	}
	otherV2 := s.group(t, "other", otherV1)
	awaitGone(t, otherV1, "the replaced leader")
	awaitGone(t, otherLeaf, "the replaced leaf")
	if !alive(worker) || statField(t, worker, 22) != workerStart {
		t.Fatalf("the unchanged worker was restarted (pid %d)", worker)
	}
	if got := s.workerGroup(t, 0); got != worker {
		t.Errorf("worker group %d, want %d", got, worker)
	}
	if !alive(otherV2) {
		t.Error("the new other is not running")
	}
	if err := d.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if err := d.Wait(); err != nil {
		t.Errorf("daemon exit: %v", err)
	}
	awaitGone(t, worker, "the worker")
	awaitGone(t, otherV2, "the new other")
}

// statField returns field n (1-based, as proc(5) numbers them) of
// /proc/<pid>/stat; 22 is the start time.
func statField(t *testing.T, pid, n int) string {
	t.Helper()
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		t.Fatal(err)
	}
	i := strings.LastIndexByte(string(b), ')')
	if i < 0 {
		t.Fatalf("stat of %d: %q", pid, b)
	}
	fields := strings.Fields(string(b[i+1:])) // field 3 onwards
	if len(fields) < n-2 {
		t.Fatalf("stat of %d: %q", pid, b)
	}
	return fields[n-3]
}

// call sends command with args to the daemon's control socket, as root.
func (s setup) call(t *testing.T, command, args string) api.Response {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req := api.Request{Version: api.Version, Command: command}
	if args != "" {
		req.Args = []byte(args)
	}
	resp, err := transport.Call(ctx, filepath.Join(s.dir, "run", "s.sock"), req)
	if err != nil {
		t.Fatalf("%s: %v", command, err)
	}
	return resp
}

// act runs an operator's command on the worker; it must be accepted.
func (s setup) act(t *testing.T, cmd api.Command) {
	t.Helper()
	if resp := s.call(t, cmd.Name, `{"name":"worker"}`); resp.Error != nil {
		t.Fatalf("%s: %v", cmd.Name, resp.Error)
	}
}

// workerStatus is the worker's supervisor status.
func (s setup) workerStatus(t *testing.T) api.ServiceStatus {
	t.Helper()
	resp := s.call(t, api.SupervisorStatus.Name, `{"name":"worker"}`)
	var st api.SupervisorStatusResult
	if resp.Error != nil || json.Unmarshal(resp.Result, &st) != nil || len(st.Services) != 1 {
		t.Fatalf("status: %+v", resp)
	}
	return st.Services[0]
}

// D-089: stop ends the whole group, a TERM-ignoring leaf included, and
// holds the service; disable outlives a restart of the daemon; enable
// leaves it stopped, start runs it again. The audit log names the
// service and links the socket's records to the module's.
func TestOperatorCommands(t *testing.T) {
	s := newSetup(t)
	d := s.startDaemon(t)
	leader, leaf := s.workerGroup(t, 0), s.leaf(t, 1)
	s.act(t, api.SupervisorStop)
	awaitGone(t, leader, "the stopped leader")
	awaitGone(t, leaf, "the stopped leaf")
	eventually(t, "the worker stopped", func() bool {
		st := s.workerStatus(t)
		return st.Phase == "stopped" && st.StoppedBy == "operator"
	})
	s.act(t, api.SupervisorDisable)

	if err := d.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if err := d.Wait(); err != nil {
		t.Errorf("daemon exit: %v", err)
	}
	s.startDaemon(t)
	eventually(t, "the daemon after a restart", func() bool {
		_, err := os.Stat(filepath.Join(s.dir, "run", "s.sock"))
		return err == nil
	})
	time.Sleep(2 * time.Second) // a launch would have happened by now
	if st := s.workerStatus(t); st.Phase != "disabled" || st.Disabled != "operator" || st.Pid != 0 {
		t.Fatalf("after the restart: %+v", st)
	}
	s.act(t, api.SupervisorEnable)
	if st := s.workerStatus(t); st.Phase != "stopped" || st.Pid != 0 {
		t.Fatalf("enable started a stopped service: %+v", st)
	}
	s.act(t, api.SupervisorStart)
	next := s.workerGroup(t, leader)
	if !alive(next) {
		t.Fatal("the worker did not start again")
	}

	resp := s.call(t, api.CoreAudit.Name, `{"limit":1000}`)
	var a api.AuditResult
	if resp.Error != nil || json.Unmarshal(resp.Result, &a) != nil {
		t.Fatalf("core.audit: %+v", resp)
	}
	var stop []string
	ids := map[string]bool{}
	for _, r := range a.Records {
		if r.Action == api.SupervisorStop.Name {
			stop = append(stop, r.Actor.Kind+"/"+r.Result+"/"+r.Target)
			ids[r.CorrelationID] = true
		}
	}
	if strings.Join(stop, " ") != "socket/intent/worker daemon/ok/worker socket/ok/worker" || len(ids) != 1 || ids[""] {
		t.Errorf("records of the stop: %v, correlation ids %v", stop, ids)
	}
}

// D-090: a program over limits.memory is stopped with its whole group
// (a TERM-ignoring leaf included) and restarted within its budget, then
// exhausted; every stop is audited with its sizes, by the daemon, in the
// incident's correlation.
func TestMemoryLimit(t *testing.T) {
	s := newSetup(t)
	writeFile(t, filepath.Join(s.dir, "etc", "supervisor", "worker.yaml"), "version: 1\nservices:\n  - name: worker\n"+
		"    command: "+s.bin+"\n    environment: {"+modeEnv+": hog}\n    output: {file: "+s.output+"}\n"+
		"    startup_grace: 0s\n    stop_timeout: 2s\n    limits: {memory: 64MiB}\n"+
		"    recovery: {max_attempts: 2, window: 1h, delay: 200ms, max_delay: 1s, stable_after: 1m, cooldown: 1m}\n")
	s.startDaemon(t)
	var st api.ServiceStatus
	for deadline := time.Now().Add(90 * time.Second); ; time.Sleep(200 * time.Millisecond) {
		if _, err := os.Stat(filepath.Join(s.dir, "run", "s.sock")); err == nil {
			if st = s.workerStatus(t); st.Phase == "exhausted" {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("not exhausted: %+v", st)
		}
	}
	if st.LastFailure != "memory_limit" || st.Failures != 3 || st.Restarts != 2 || st.Pid != 0 {
		t.Errorf("status: %+v", st)
	}
	if n := s.runs(); n != 3 {
		t.Errorf("%d runs, want 3", n)
	}
	for i := 1; i <= 3; i++ {
		if leaf := s.leaf(t, i); alive(leaf) {
			t.Errorf("the leaf of run %d survived its group's stop", i)
		}
	}
	resp := s.call(t, api.CoreAudit.Name, `{"limit":1000}`)
	var a api.AuditResult
	if resp.Error != nil || json.Unmarshal(resp.Result, &a) != nil {
		t.Fatalf("core.audit: %+v", resp)
	}
	var stops []string
	ids := map[string]bool{}
	for _, r := range a.Records {
		if r.Action != api.SupervisorStop.Name {
			continue
		}
		stops = append(stops, r.Actor.Kind+"/"+r.Result+"/"+r.Target)
		ids[r.CorrelationID] = true
		if r.Detail["trigger"] != "memory_limit" || r.Detail["limit_bytes"] != "67108864" {
			t.Errorf("stop record %+v", r)
		}
		if rss, _ := strconv.ParseUint(r.Detail["rss_bytes"], 10, 64); rss <= 64<<20 {
			t.Errorf("stop record with rss_bytes %q", r.Detail["rss_bytes"])
		}
	}
	want := strings.TrimSpace(strings.Repeat("daemon/intent/worker daemon/ok/worker ", 3))
	if strings.Join(stops, " ") != want || len(ids) != 1 || ids[""] {
		t.Errorf("stop records %v, correlation ids %v", stops, ids)
	}
}
