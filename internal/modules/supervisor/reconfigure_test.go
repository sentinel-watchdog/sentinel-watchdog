package supervisor

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/module"
	"github.com/sentinel-watchdog/sentinel-watchdog/internal/platform/executor"
)

// procDef is a process service that runs /bin/sh with arg, routed to
// channel, with extra YAML lines appended.
func procDef(name, arg, channel, extra string) string {
	return "  - name: " + name + "\n    command: /bin/sh\n    args: [\"" + arg + "\"]\n    startup_grace: 1s\n" +
		"    notifications: [" + channel + "]\n" + extra
}

// procFiles is a module file of process services; central, when not
// empty, replaces armedCentral.
func procFiles(central string, defs ...string) map[string]string {
	files := map[string]string{"supervisor/a.yaml": "version: 1\nservices:\n" + strings.Join(defs, "")}
	if central != "" {
		files["sentinel.yaml"] = central
	}
	return files
}

// launchesOf returns the indexes of l's launches whose first argument is
// arg, in launch order.
func launchesOf(l *fakeLauncher, arg string) []int {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []int
	for i, s := range l.specs {
		if len(s.Args) > 0 && s.Args[0] == arg {
			out = append(out, i)
		}
	}
	return out
}

// reconfigureTo configures files with l and offers them to r's instance.
func reconfigureTo(t *testing.T, ctx context.Context, r *rig, l *fakeLauncher, files map[string]string, secrets ...string) error {
	t.Helper()
	next := mustConfigureProcs(t, files, l)
	return r.in.Reconfigure(ctx, next, module.Runtime{Secrets: secrets})
}

// D-088: a reload that changes one service stops and starts only that
// one; the other keeps its process, pid and loop. A changed definition
// starts again from a fresh state (D-082).
func TestReconfigureKeepsUnchangedProcesses(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := &fakeLauncher{}
		in := mustConfigureProcs(t, procFiles("", procDef("a", "a1", "ops", ""), procDef("b", "b1", "ops", "")), l)
		r := start(t, in, "")
		defer r.stop(t)
		tick(2 * time.Second)
		bPid := statusOf(t, in, "b").Pid
		if err := reconfigureTo(t, t.Context(), r, l, procFiles("", procDef("a", "a2", "ops", ""), procDef("b", "b1", "ops", ""))); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		a1, a2, b := launchesOf(l, "a1"), launchesOf(l, "a2"), launchesOf(l, "b1")
		if len(a1) != 1 || len(a2) != 1 || len(b) != 1 {
			t.Fatalf("launches a1 %v a2 %v b %v", a1, a2, b)
		}
		if stops := l.proc(a1[0]).stopCalls(); len(stops) != 1 {
			t.Errorf("old a stopped %d times", len(stops))
		}
		if stops := l.proc(b[0]).stopCalls(); len(stops) != 0 {
			t.Errorf("unchanged b was stopped: %v", stops)
		}
		if s := statusOf(t, in, "b"); s.Pid != bPid || s.Phase != phaseRunning || s.Starts != 1 {
			t.Errorf("b after the reload: %+v", s)
		}
		if s := statusOf(t, in, "a"); s.Phase != phaseStarting || s.State != "unknown" || s.Starts != 1 {
			t.Errorf("a after the reload: %+v", s)
		}
		if g := readFileState(t, r.stateDir).Groups["a"]; g.Pgid != 1000+a2[0]+1 {
			t.Errorf("a's record is not the new group: %+v", g)
		}
	})
}

// D-088: a route change alone restarts nothing and keeps the incident;
// the next event goes to the new channel.
func TestReconfigureRouteOnly(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := &fakeLauncher{}
		in := mustConfigureProcs(t, procFiles("", procDef("b", "b1", "ops", "")), l)
		r := start(t, in, "")
		defer r.stop(t)
		tick(2 * time.Second)
		if err := reconfigureTo(t, t.Context(), r, l, procFiles("", procDef("b", "b1", "pager", ""))); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		if l.launches() != 1 || len(l.proc(0).stopCalls()) != 0 {
			t.Fatalf("a route change restarted the process: %d launches", l.launches())
		}
		if got := in.NotificationChannels(eventFor("b", string(EventServiceFailed))); !slices.Equal(got, []string{"pager"}) {
			t.Errorf("routes %v", got)
		}
	})
}

// Removing a service stops it and drops its state; adding one starts it.
func TestReconfigureRemovesAndAdds(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := &fakeLauncher{}
		in := mustConfigureProcs(t, procFiles("", procDef("a", "a1", "ops", ""), procDef("b", "b1", "ops", "")), l)
		r := start(t, in, "")
		defer r.stop(t)
		tick(2 * time.Second)
		if err := reconfigureTo(t, t.Context(), r, l, procFiles("", procDef("b", "b1", "ops", ""), procDef("c", "c1", "ops", ""))); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		if a := launchesOf(l, "a1"); len(l.proc(a[0]).stopCalls()) != 1 {
			t.Error("removed a was not stopped")
		}
		if c := launchesOf(l, "c1"); len(c) != 1 {
			t.Errorf("c launches %v", c)
		}
		if _, apiErr := in.status("a"); apiErr == nil {
			t.Error("removed a still in status")
		}
		f := readFileState(t, r.stateDir)
		if _, ok := f.Services["a"]; ok {
			t.Error("removed a's state kept")
		}
		if _, ok := f.Groups["a"]; ok {
			t.Error("removed a's group record kept after a clean stop")
		}
	})
}

// Q5 of 3b-4: a recovery policy edit restarts the service but keeps its
// budget and its incident: no free attempt, the incident stays open.
func TestReconfigureKeepsTheBudgetOfAPolicyEdit(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := &fakeLauncher{}
		in := mustConfigureProcs(t, loopFiles(), l)
		r := start(t, in, "")
		defer r.stop(t)
		synctest.Wait()
		crash(l, 0)
		tick(time.Second) // the first attempt, after 1s
		before := statusOf(t, in, "worker")
		if before.Attempts != 1 || before.IncidentID == "" {
			t.Fatalf("before: %+v", before)
		}
		edited := strings.Replace(loopService, "max_attempts: 3", "max_attempts: 4", 1)
		if err := reconfigureTo(t, t.Context(), r, l, map[string]string{"supervisor/a.yaml": edited}); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		after := statusOf(t, in, "worker")
		if after.IncidentID != before.IncidentID || after.State != "failed" || after.MaxAttempts != 4 {
			t.Errorf("after: %+v", after)
		}
		// The relaunch with an open incident is an attempt, after the
		// second attempt's delay.
		if after.Attempts != 1 || l.launches() != 2 {
			t.Errorf("before the delay: attempts %d, launches %d", after.Attempts, l.launches())
		}
		tick(2 * time.Second)
		if after := statusOf(t, in, "worker"); after.Attempts != 2 || l.launches() != 3 {
			t.Errorf("attempts %d, launches %d", after.Attempts, l.launches())
		}
	})
}

// Q3 of 3b-4: a process that does not stop for a reload keeps running,
// owned by the instance; its name takes no new definition, through later
// reloads, a removal and a re-addition; Stop still stops it.
func TestReconfigureBlocksAProcessThatDoesNotStop(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := &fakeLauncher{}
		in := mustConfigureProcs(t, procFiles("", procDef("a", "a1", "ops", ""), procDef("b", "b1", "ops", "")), l)
		r := start(t, in, "")
		tick(2 * time.Second)
		old := l.proc(launchesOf(l, "a1")[0])
		old.mu.Lock()
		old.stubborn = true
		old.mu.Unlock()
		reload := func(files map[string]string) error {
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			return reconfigureTo(t, ctx, r, l, files)
		}
		err := reload(procFiles("", procDef("a", "a2", "ops", ""), procDef("b", "b2", "ops", "")))
		if err == nil || !strings.Contains(err.Error(), "a") || strings.Contains(err.Error(), "b") {
			t.Fatalf("Reconfigure: %v", err)
		}
		synctest.Wait()
		if s := statusOf(t, in, "a"); s.Phase != phaseBlocked || s.BlockedReason != blockedReload {
			t.Errorf("a: %+v", s)
		}
		if len(launchesOf(l, "b2")) != 1 {
			t.Error("b was not replaced beside a's failure")
		}
		for _, files := range []map[string]string{
			procFiles("", procDef("a", "a3", "ops", ""), procDef("b", "b2", "ops", "")),
			procFiles("", procDef("b", "b2", "ops", "")),                                // removed
			procFiles("", procDef("a", "a4", "ops", ""), procDef("b", "b2", "ops", "")), // added again
		} {
			if err := reload(files); err == nil || !strings.Contains(err.Error(), "a") {
				t.Errorf("a later reload does not report a as blocked: %v", err)
			}
			synctest.Wait()
		}
		if n := len(launchesOf(l, "a2")) + len(launchesOf(l, "a3")) + len(launchesOf(l, "a4")); n != 0 {
			t.Errorf("a launched %d times beside its old process", n)
		}
		if f := readFileState(t, r.stateDir); f.Groups["a"].Pgid != 1000+launchesOf(l, "a1")[0]+1 {
			t.Errorf("a's record is not the old process's: %+v", f.Groups["a"])
		}
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		if err := in.Stop(ctx); err == nil || !strings.Contains(err.Error(), "1 processes did not end") {
			t.Errorf("Stop: %v", err)
		}
		if len(old.stopCalls()) < 2 {
			t.Error("Stop did not stop the process the reload left")
		}
	})
}

// Review 3b-4 C1: an http check that ignores its cancellation keeps its
// service's name: no second check of it starts, however many reloads.
func TestReconfigureBlocksAStalledCheck(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		release := make(chan struct{})
		fs := newFakes()
		files := func(interval string) map[string]string {
			return map[string]string{"supervisor/a.yaml": "version: 1\nservices:\n  - name: web\n    url: https://web.example.org\n" +
				"    check_interval: " + interval + "\n    timeout: 1s\n"}
		}
		in, err := configure(t, files("10s"), fs.build)
		if err != nil {
			t.Fatal(err)
		}
		fs.get("web").set(func(context.Context) Result { <-release; return Result{} })
		r := start(t, in, "")
		synctest.Wait()
		for _, interval := range []string{"20s", "30s", "40s"} {
			next, err := configure(t, files(interval), fs.build)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
			err = in.Reconfigure(ctx, next, module.Runtime{})
			cancel()
			if err == nil {
				t.Fatalf("interval %s: a stalled check was left behind", interval)
			}
			synctest.Wait()
		}
		if n := fs.get("web").count(); n != 1 {
			t.Errorf("%d checks of web started", n)
		}
		if s := statusOf(t, in, "web"); s.BlockedReason != blockedReload {
			t.Errorf("web: %+v", s)
		}
		close(release)
		r.stop(t)
	})
}

// Q7 of 3b-4: turning the central switch off stops every process; on
// again starts them with the usual admission.
func TestReconfigureCentralSwitch(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := &fakeLauncher{}
		def := procDef("a", "a1", "ops", "")
		in := mustConfigureProcs(t, procFiles("", def), l)
		r := start(t, in, "")
		defer r.stop(t)
		tick(2 * time.Second)
		if err := reconfigureTo(t, t.Context(), r, l, procFiles(centralFile, def)); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		if len(l.proc(0).stopCalls()) != 1 {
			t.Error("the process was not stopped when disarmed")
		}
		if s := statusOf(t, in, "a"); s.Phase != phaseNotArmed {
			t.Errorf("disarmed: %+v", s)
		}
		if err := reconfigureTo(t, t.Context(), r, l, procFiles("", def)); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		if l.launches() != 2 {
			t.Errorf("%d launches after arming again", l.launches())
		}
	})
}

// Q8 of 3b-4: a new max_concurrent_checks asks the daemon for a restart,
// before anything changes.
func TestReconfigureCapacityAsksForARestart(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := &fakeLauncher{}
		def := procDef("a", "a1", "ops", "")
		in := mustConfigureProcs(t, procFiles("", def), l)
		r := start(t, in, "")
		defer r.stop(t)
		synctest.Wait()
		files := procFiles("", def)
		files["supervisor/s.yaml"] = "version: 1\nsettings:\n  max_concurrent_checks: 3\n"
		if err := reconfigureTo(t, t.Context(), r, l, files); !errors.Is(err, module.ErrRestart) {
			t.Fatalf("Reconfigure: %v", err)
		}
		if len(l.proc(0).stopCalls()) != 0 {
			t.Error("ErrRestart after a change")
		}
	})
}

// Review 3b-4 C2: a loop for a name waits for the cleanup that owns its
// record, and a cleanup forgets only the record it cleaned.
func TestReconfigureCleanupOwnership(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		gate := make(chan struct{})
		l := &fakeLauncher{cleanupGate: gate}
		in := mustConfigureProcs(t, procFiles("", procDef("b", "b1", "ops", "")), l)
		stateDir := t.TempDir()
		stale := groupRecord{Pgid: 4242, Sid: 50, Start: 7, Boot: testBoot}
		writeStateFile(t, stateDir, fmt.Sprintf(`{"services":{},"groups":{"a":{"pgid":%d,"sid":%d,"start":%d,"boot":%q}}}`,
			stale.Pgid, stale.Sid, stale.Start, stale.Boot))
		r := start(t, in, stateDir)
		defer r.stop(t)
		synctest.Wait() // the stale cleanup of a holds its gate
		if err := reconfigureTo(t, t.Context(), r, l, procFiles("", procDef("a", "a1", "ops", ""), procDef("b", "b1", "ops", ""))); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		if n := len(launchesOf(l, "a1")); n != 0 {
			t.Fatalf("a launched while a cleanup owned its record (%d)", n)
		}
		close(gate)
		synctest.Wait()
		if n := len(launchesOf(l, "a1")); n != 1 {
			t.Fatalf("a launches after the cleanup: %d", n)
		}
		if g := readFileState(t, r.stateDir).Groups["a"]; g.Pgid == stale.Pgid || g.Pgid == 0 {
			t.Errorf("a's record %+v", g)
		}
		n := 0
		for _, g := range l.cleaned() {
			if g.Pgid == stale.Pgid {
				n++
			}
		}
		if n != 1 {
			t.Errorf("the stale group was cleaned up %d times: two cleanups raced", n)
		}
	})
}

// A launch that completes after a reload gave up retiring its service is
// stopped by its own loop at once, not left until Stop.
func TestReconfigureStopsALateLaunch(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		gate := make(chan struct{})
		l := &fakeLauncher{gate: gate}
		in := mustConfigureProcs(t, procFiles("", procDef("a", "a1", "ops", "")), l)
		r := start(t, in, "")
		defer r.stop(t)
		synctest.Wait() // a's first launch waits in Launch
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		if err := reconfigureTo(t, ctx, r, l, procFiles("", procDef("a", "a2", "ops", ""))); err == nil {
			t.Fatal("a loop stuck in Launch was retired")
		}
		close(gate)
		synctest.Wait()
		if stops := l.proc(launchesOf(l, "a1")[0]).stopCalls(); len(stops) != 1 {
			t.Errorf("the late launch was stopped %d times", len(stops))
		}
		if n := len(launchesOf(l, "a2")); n != 0 {
			t.Errorf("a2 launched beside a1 (%d)", n)
		}
	})
}

// Review 3b-4 C2: a cleanup that completes after the name got a newer
// record leaves that record.
func TestCleanupKeepsANewerRecord(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		gate := make(chan struct{})
		l := &fakeLauncher{}
		in := mustConfigureProcs(t, procFiles("", procDef("b", "b1", "ops", "")), l)
		r := start(t, in, "")
		defer r.stop(t)
		synctest.Wait()
		oldRec, newRec := groupRecord{Pgid: 10, Sid: 50, Start: 1, Boot: testBoot}, groupRecord{Pgid: 11, Sid: 50, Start: 2, Boot: testBoot}
		in.mu.Lock()
		in.groups["x"] = oldRec
		in.mu.Unlock()
		l.mu.Lock()
		l.cleanupGate = gate
		l.mu.Unlock()
		done := make(chan bool, 1)
		go func() { done <- in.cleanupRecord(t.Context(), "x") }()
		synctest.Wait()
		in.mu.Lock()
		in.groups["x"] = newRec
		in.mu.Unlock()
		close(gate)
		if !<-done {
			t.Fatal("cleanup failed")
		}
		in.mu.Lock()
		got := in.groups["x"]
		delete(in.groups, "x")
		in.mu.Unlock()
		if got != newRec {
			t.Errorf("record %+v after the old cleanup", got)
		}
	})
}

// Review 3b-4 C4: a launch keeps masking the secrets of the configuration
// it was launched under after a reload changed them; a new launch masks the
// new ones.
func TestReconfigureRedactionPerLaunch(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := &fakeLauncher{}
		in := mustConfigureProcs(t, procFiles("", procDef("a", "a1", "ops", ""), procDef("b", "b1", "ops", "")), l)
		r := start(t, in, "")
		defer r.stop(t)
		synctest.Wait()
		// a2 is launched under a configuration whose secret is old-secret.
		if err := reconfigureTo(t, t.Context(), r, l, procFiles("", procDef("a", "a2", "ops", ""), procDef("b", "b1", "ops", "")),
			"old-config-secret"); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		l.mu.Lock()
		l.output = []string{"b2 says new-config-secret"}
		l.mu.Unlock()
		// a2 is kept; b2 is launched under the new secret only.
		if err := reconfigureTo(t, t.Context(), r, l, procFiles("", procDef("a", "a2", "ops", ""), procDef("b", "b2", "ops", "")),
			"new-config-secret"); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		_, _ = fmt.Fprintln(l.spec(launchesOf(l, "a2")[0]).Stdout, "a2 says old-config-secret")
		synctest.Wait()
		logs := r.logs.String()
		if strings.Contains(logs, "old-config-secret") || strings.Contains(logs, "new-config-secret") {
			t.Errorf("a secret in the output log:\n%s", logs)
		}
		if !strings.Contains(logs, "a2 says") || !strings.Contains(logs, "b2 says") {
			t.Errorf("output not logged:\n%s", logs)
		}
	})
}

// Q2 of 3b-4: status may run beside a reconfiguration (-race).
func TestReconfigureBesideStatus(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := &fakeLauncher{}
		in := mustConfigureProcs(t, procFiles("", procDef("a", "a1", "ops", "")), l)
		r := start(t, in, "")
		defer r.stop(t)
		synctest.Wait()
		ctx, cancel := context.WithCancel(t.Context())
		var wg sync.WaitGroup
		wg.Go(func() {
			for ctx.Err() == nil {
				_, _ = in.Handle(ctx, "supervisor.status", nil)
				_ = in.NotificationChannels(eventFor("a", string(EventServiceFailed)))
				time.Sleep(time.Millisecond)
			}
		})
		for i := range 5 {
			if err := reconfigureTo(t, t.Context(), r, l, procFiles("", procDef("a", fmt.Sprint("a", i+2), "ops", ""))); err != nil {
				t.Fatal(err)
			}
			tick(10 * time.Millisecond)
		}
		cancel()
		wg.Wait()
		if l.launches() != 6 {
			t.Errorf("%d launches", l.launches())
		}
	})
}

// Review 3b-4 C5: a service a reload could not stop is not in the state
// file, so a full configuration beside it still writes a valid one.
func TestReconfigureKeepsTheStateBounded(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := &fakeLauncher{}
		defs := func(from int) []string {
			var out []string
			for i := from; i < from+maxServices; i++ {
				out = append(out, procDef(fmt.Sprintf("s%03d", i), fmt.Sprintf("x%03d", i), "ops", ""))
			}
			return out
		}
		in := mustConfigureProcs(t, procFiles("", defs(0)...), l)
		r := start(t, in, "")
		synctest.Wait()
		p := l.proc(launchesOf(l, "x000")[0])
		p.mu.Lock()
		p.stubborn = true
		p.mu.Unlock()
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		if err := reconfigureTo(t, ctx, r, l, procFiles("", defs(1)...)); err == nil {
			t.Fatal("s000 did not block")
		}
		synctest.Wait()
		f := readFileState(t, r.stateDir)
		if err := f.Validate(); err != nil || len(f.Services) != maxServices {
			t.Errorf("state: %d services, %v", len(f.Services), err)
		}
		p.end(executor.Exit{}, nil)
		r.stop(t)
	})
}
