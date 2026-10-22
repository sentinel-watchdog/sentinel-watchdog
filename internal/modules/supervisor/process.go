package supervisor

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/redact"
	"github.com/sentinel-watchdog/sentinel-watchdog/internal/platform/executor"
	"github.com/sentinel-watchdog/sentinel-watchdog/pkg/model"
)

// Phases of a process service, shown by status.
const (
	phaseNotArmed    = "not_armed"    // modules.supervisor.processes is false
	phaseStarting    = "starting"     // running, within startup_grace
	phaseRunning     = "running"      // running past startup_grace
	phaseExited      = "exited"       // ended by itself; not restarted (3b-2)
	phaseStartFailed = "start_failed" // could not be started
	phaseStopping    = "stopping"     // being stopped by sentineld
	phaseStopped     = "stopped"      // stopped by sentineld
)

const (
	// stableAfter is how long a relaunched process must run before its
	// open incident closes with service_recovered: the default of
	// recovery.stable_after (D-084 (5)), configurable from 3b-3 (D-086).
	stableAfter = time.Minute
	// cleanupTimeout bounds the cleanup of one recorded group.
	cleanupTimeout = 5 * time.Second
)

// runProcess is the life of one process service: clean up what a
// previous run left of its group, launch it once if the central switch
// allows, and watch it until it ends or the module stops. There is no
// restart in 3b-2. A panic in the module's own code is logged without its
// value; the process, if launched, is still stopped by Stop.
func (in *instance) runProcess(ctx context.Context, s *service) {
	defer in.workers.Done()
	defer func() {
		if recover() != nil {
			in.log.Error("supervisor process loop panicked", "service", s.def.Name)
		}
	}()
	if !in.cleanupRecord(ctx, s.def.Name) {
		if ctx.Err() == nil {
			in.processFailed(s, FailureCleanup, nil, false)
		}
		return
	}
	if !in.processes {
		in.setPhase(s, phaseNotArmed)
		return
	}
	lp, sinks, ok := in.launchProcess(ctx, s)
	if !ok {
		return
	}
	in.watch(ctx, s, lp, sinks)
}

// cleanupRecord kills what is left of the group recorded for name, if
// any, and forgets the record once nothing of it is alive. It reports
// whether the name is free to launch.
func (in *instance) cleanupRecord(ctx context.Context, name string) bool {
	in.mu.Lock()
	rec, ok := in.groups[name]
	in.mu.Unlock()
	if !ok {
		return true
	}
	cctx, cancel := context.WithTimeout(ctx, cleanupTimeout)
	defer cancel()
	if err := in.launcher.Cleanup(cctx, rec.group()); err != nil {
		if ctx.Err() == nil {
			in.log.Error("supervisor could not clean up the process group a previous run left; the service is not started",
				"service", name, "pgid", rec.Pgid, "error", err)
		}
		return false
	}
	in.mu.Lock()
	delete(in.groups, name)
	in.gen++
	gen := in.gen
	in.mu.Unlock()
	in.save(gen)
	in.log.Info("supervisor cleaned up the process group a previous run left", "service", name, "pgid", rec.Pgid)
	return true
}

// cleanupStale cleans up the records of names that are no longer process
// services (removed, renamed, changed to http). A failure keeps the
// record for the next start.
func (in *instance) cleanupStale(ctx context.Context, names []string) {
	defer in.workers.Done()
	for _, name := range names {
		if ctx.Err() != nil {
			return
		}
		in.cleanupRecord(ctx, name)
	}
}

// launchProcess starts s's process. It owns the process before it looks
// at ctx again: a launch that completes while the module stops is stopped
// by Stop like any other.
func (in *instance) launchProcess(ctx context.Context, s *service) (launched, []io.Closer, bool) {
	spec, sinks, code := in.prepare(s)
	if code != "" {
		in.processFailed(s, code, nil, true)
		return launched{}, nil, false
	}
	in.mu.Lock()
	full := len(in.groups) >= maxGroups
	in.mu.Unlock()
	if full {
		closeAll(sinks)
		in.log.Error("supervisor keeps too many process group records; the service is not started", "service", s.def.Name)
		in.processFailed(s, FailureCleanup, nil, true)
		return launched{}, nil, false
	}
	lp, err := in.launcher.Launch(ctx, spec)
	if err != nil {
		closeAll(sinks)
		if ctx.Err() == nil {
			in.log.Error("supervisor could not start the process", "service", s.def.Name, "error", in.redact(err.Error()))
			in.processFailed(s, launchFailure(err), nil, true)
		}
		return launched{}, nil, false
	}
	now := in.rt.Clock.Now()
	in.mu.Lock()
	s.proc, s.pid, s.startedAt, s.phase, s.sinks = lp.proc, lp.pid, now, phaseStarting, sinks
	s.st.Starts++
	if !lp.group.IsZero() {
		in.groups[s.def.Name] = recordOf(lp.group)
	}
	in.gen++
	gen := in.gen
	in.mu.Unlock()
	in.save(gen) // the record first: a crash from now on leaves it for the next run
	in.log.Info("supervisor started the process", "service", s.def.Name, "pid", lp.pid)
	return lp, sinks, true
}

// prepare builds the Spec of s's process for this launch: the program
// resolved and checked again (it may have changed since Configure), and
// the output sinks opened. code is the failure when it cannot.
func (in *instance) prepare(s *service) (executor.Spec, []io.Closer, Failure) {
	pr := s.def.Process
	path, err := executor.TrustedExecutable(pr.Command)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		in.log.Error("supervisor cannot start the process: the program is missing", "service", s.def.Name, "error", err)
		return executor.Spec{}, nil, FailureNotFound
	case err != nil:
		in.log.Error("supervisor refuses to start the process: the program is not trusted", "service", s.def.Name, "error", err)
		return executor.Spec{}, nil, FailureUntrusted
	}
	spec := executor.Spec{Path: path, Arg0: pr.Command, Args: pr.Args, Dir: pr.WorkingDir, Env: pr.Env, Cred: pr.Cred}
	var sinks []io.Closer
	switch pr.Output.Kind {
	case outputLog:
		for _, stream := range []string{"stdout", "stderr"} {
			w := executor.NewLineWriter(in.rt.Clock, executor.DefaultLineOptions, func(line string) {
				in.log.Info("service output", "service", s.def.Name, "stream", stream, "line", in.redact(line))
			})
			if stream == "stdout" {
				spec.Stdout = w
			} else {
				spec.Stderr = w
			}
			sinks = append(sinks, w)
		}
	case outputFile:
		f, err := executor.OpenAppend(pr.Output.File, pr.Output.Mode)
		if err != nil {
			in.log.Error("supervisor cannot open the output file", "service", s.def.Name, "error", err)
			return executor.Spec{}, nil, FailureOutput
		}
		spec.Stdout, spec.Stderr = f, f
		sinks = append(sinks, f)
	}
	return spec, sinks, ""
}

// launchFailure classifies a start error.
func launchFailure(err error) Failure {
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return FailureNotFound
	case errors.Is(err, fs.ErrPermission), errors.Is(err, syscall.EPERM):
		return FailurePermission
	case errors.Is(err, executor.ErrUnsupported):
		return FailureUnsupported
	}
	return FailureExec
}

// watch follows a launched process until it ends or the module stops.
func (in *instance) watch(ctx context.Context, s *service, lp launched, sinks []io.Closer) {
	grace := in.rt.Clock.NewTimer(s.def.Process.StartupGrace)
	defer grace.Stop()
	stable := in.rt.Clock.NewTimer(stableAfter)
	defer stable.Stop()
	starting := true
	for {
		select {
		case <-lp.proc.Done():
			if ctx.Err() != nil {
				return // shutdown is not a failure; Stop records the end
			}
			closeAll(sinks)
			exit, outErr := lp.proc.Result()
			in.processExited(s, exit, outErr, starting)
			return
		case <-grace.C():
			starting = false
			in.passedGrace(s)
		case <-stable.C():
			in.stable(s)
		case <-ctx.Done():
			return
		}
	}
}

// passedGrace marks s running; a service seen for the first time, or
// healthy, is healthy now. A failed one stays failed until stableAfter.
func (in *instance) passedGrace(s *service) {
	now := in.rt.Clock.Now()
	in.mu.Lock()
	if s.phase == phaseStarting {
		s.phase = phaseRunning
	}
	changed := false
	if s.st.Health == HealthUnknown {
		s.prev, s.st.Health, s.st.Since = s.st.Health, HealthHealthy, now
		changed = true
	}
	in.gen++
	gen := in.gen
	in.mu.Unlock()
	if changed {
		in.save(gen)
	}
}

// stable closes the open incident of a process that has run for
// stableAfter (D-084 (6)).
func (in *instance) stable(s *service) {
	now := in.rt.Clock.Now()
	in.mu.Lock()
	if s.st.Health != HealthFailed {
		in.mu.Unlock()
		return
	}
	incident, since := s.st.IncidentID, s.st.Since
	s.prev, s.st.Health, s.st.IncidentID, s.st.Since = HealthFailed, HealthHealthy, "", now
	e := processRecoveredEvent(s, incident, now.Sub(since))
	in.gen++
	gen := in.gen
	in.mu.Unlock()
	in.save(gen)
	in.publish(s, e)
}

// processExited records the unrequested end of s's process: always a
// failure, status 0 included (D-084 (6)).
func (in *instance) processExited(s *service, exit executor.Exit, outErr error, startup bool) {
	code := FailureExited
	if exit.Signal != "" {
		code = FailureKilled
	}
	in.log.Warn("supervisor process ended", "service", s.def.Name, "exit", exit.String(), "during_startup", startup)
	in.processFailed(s, code, &exit, startup)
	in.mu.Lock()
	s.outputLost = outErr != nil
	in.mu.Unlock()
}

// processFailed records a failure of s: a start that failed, or a process
// that ended. The first failure of an incident emits service_failed;
// while an incident is open, later ones only count. State first, then the
// event, as observe does.
func (in *instance) processFailed(s *service, code Failure, exit *executor.Exit, startup bool) {
	now := in.rt.Clock.Now()
	in.mu.Lock()
	st := &s.st
	st.Failures++
	st.LastFailure = code
	if exit != nil {
		st.LastExit = &exitRecord{Code: exit.Code, Signal: exit.Signal, At: now}
		s.phase = phaseExited
		s.proc, s.pid, s.startedAt, s.sinks = nil, 0, time.Time{}, nil
		delete(in.groups, s.def.Name) // the executor killed the rest of the group
	} else {
		s.phase = phaseStartFailed
	}
	var ev *model.Event
	if st.Health != HealthFailed {
		s.prev = st.Health
		st.Health, st.IncidentID, st.Since = HealthFailed, model.NewEventID(), now
		e := processFailedEvent(s, st.IncidentID, exit, startup)
		ev = &e
	}
	in.gen++
	gen := in.gen
	in.mu.Unlock()
	in.save(gen)
	if ev != nil {
		in.publish(s, *ev)
	}
}

// publish publishes e, logging a failure.
func (in *instance) publish(s *service, e model.Event) {
	if _, err := in.rt.Events.Publish(e); err != nil {
		in.log.Error("supervisor event not published", "service", s.def.Name, "event_type", e.Type, "error", err)
	}
}

func (in *instance) setPhase(s *service, phase string) {
	in.mu.Lock()
	s.phase = phase
	in.mu.Unlock()
}

// stopProcesses stops every process the module owns, in parallel, each
// with its stop signal and timeout under ctx (the executor kills the group
// when ctx ends first). A process sentineld stops is not a failure. It
// returns how many did not end; their records stay for the next start.
func (in *instance) stopProcesses(ctx context.Context) int {
	type owned struct {
		s     *service
		p     proc
		sinks []io.Closer
	}
	var all []owned
	in.mu.Lock()
	for _, s := range in.services {
		if s.proc != nil {
			all = append(all, owned{s, s.proc, s.sinks})
			s.phase = phaseStopping
		}
	}
	in.mu.Unlock()
	results := make(chan bool, len(all))
	for _, o := range all {
		go func() {
			pr := o.s.def.Process
			exit, err := o.p.Stop(ctx, pr.StopSignal, pr.StopTimeout)
			ended := err == nil
			if ended {
				closeAll(o.sinks)
				in.stopped(o.s, exit)
			} else {
				in.log.Error("supervisor process did not end when stopped", "service", o.s.def.Name, "error", err)
			}
			results <- ended
		}()
	}
	left := 0
	for range all {
		if !<-results {
			left++
		}
	}
	return left
}

// stopped records the end of a process sentineld stopped.
func (in *instance) stopped(s *service, exit executor.Exit) {
	now := in.rt.Clock.Now()
	in.mu.Lock()
	defer in.mu.Unlock()
	s.st.LastExit = &exitRecord{Code: exit.Code, Signal: exit.Signal, At: now}
	s.phase = phaseStopped
	s.proc, s.pid, s.startedAt, s.sinks = nil, 0, time.Time{}, nil
	delete(in.groups, s.def.Name)
	in.gen++
}

func closeAll(cs []io.Closer) {
	for _, c := range cs {
		_ = c.Close() // a log line writer never fails; an output file is append-only
	}
}

// processRedactor masks, in a process's output, the configuration's
// secrets (configRedact) and the values of every process service's
// environment, which may be literal tokens the configuration does not know
// as secrets (D-086). Values shorter than four bytes are left alone, as by
// redact.Text; arguments and scripts are not masked: secrets belong in the
// environment.
func processRedactor(configRedact func(string) string, services []*service) func(string) string {
	var secrets []string
	for _, s := range services {
		for _, kv := range s.def.Process.Env {
			if k, v, _ := strings.Cut(kv, "="); k != "PATH" {
				secrets = append(secrets, v)
			}
		}
	}
	// Longer first: a secret that contains another is masked whole.
	slices.SortFunc(secrets, func(a, b string) int { return len(b) - len(a) })
	return func(text string) string {
		if configRedact != nil {
			text = configRedact(text)
		}
		return redact.Text(text, secrets...)
	}
}
