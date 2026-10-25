package supervisor

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"time"

	"github.com/sentinel-watchdog/sentinel-watchdog/pkg/api"
)

// Handle implements module.Handler. The read commands copy the state
// under the lock and never wait for a check or for the state file; the
// operator's commands (operate.go) wait for their action a bounded time.
func (in *instance) Handle(ctx context.Context, command string, args jsontext.Value) (any, *api.Error) {
	if act, ok := actions[command]; ok {
		name, apiErr := actionName(command, args)
		if apiErr != nil {
			return nil, apiErr
		}
		return in.act(ctx, name, act)
	}
	switch command {
	case api.SupervisorList.Name:
		if !emptyArgs(args) {
			return nil, &api.Error{Code: api.CodeBadRequest, Message: "supervisor.list takes no arguments"}
		}
		return in.list(), nil
	case api.SupervisorStatus.Name:
		var a api.SupervisorStatusArgs
		if !emptyArgs(args) {
			if err := json.Unmarshal(args, &a, json.RejectUnknownMembers(true)); err != nil {
				return nil, &api.Error{Code: api.CodeBadRequest, Message: "supervisor.status takes {\"name\": <service>} or nothing"}
			}
		}
		return in.status(a.Name)
	}
	return nil, &api.Error{Code: api.CodeUnknownCommand, Message: "unknown supervisor command"}
}

// actionName returns the service an operator's command names: its only
// argument, a valid name (D-011).
func actionName(command string, args jsontext.Value) (string, *api.Error) {
	var a api.SupervisorActionArgs
	if json.Unmarshal(args, &a, json.RejectUnknownMembers(true)) != nil || !api.ValidName(a.Name) {
		return "", &api.Error{Code: api.CodeBadRequest, Message: command + " takes {\"name\": <service>}"}
	}
	return a.Name, nil
}

// emptyArgs reports whether args is absent or {}.
func emptyArgs(args jsontext.Value) bool {
	if len(args) == 0 {
		return true
	}
	var m map[string]jsontext.Value
	return json.Unmarshal(args, &m) == nil && len(m) == 0
}

func (in *instance) list() api.SupervisorListResult {
	in.mu.Lock()
	defer in.mu.Unlock()
	out := api.SupervisorListResult{Services: make([]api.ServiceSummary, 0, len(in.services))}
	for _, s := range in.services {
		out.Services = append(out.Services, api.ServiceSummary{Name: s.def.Name, Type: s.def.Type,
			State: string(s.st.Health), Since: s.st.Since, Disabled: s.def.Disabled || s.st.Disabled})
	}
	return out
}

func (in *instance) status(name string) (api.SupervisorStatusResult, *api.Error) {
	if name != "" && !api.ValidName(name) {
		return api.SupervisorStatusResult{}, &api.Error{Code: api.CodeBadRequest, Message: "invalid service name"}
	}
	in.mu.Lock()
	defer in.mu.Unlock()
	out := api.SupervisorStatusResult{MaxConcurrentChecks: in.settings.MaxConcurrentChecks,
		ChecksInFlight: int(in.running.Load()), StateNotSaved: in.notSaved, ProcessesArmed: in.processes,
		PendingCleanups: len(in.groups) - in.runningProcesses(), StateRecovered: in.stateRecovered,
		Services: []api.ServiceStatus{}}
	now := in.rt.Clock.Now()
	for _, s := range in.services {
		if name != "" && s.def.Name != name {
			continue
		}
		if s.def.Type == typeProcess {
			out.Services = append(out.Services, processStatus(s, now))
			continue
		}
		out.Services = append(out.Services, api.ServiceStatus{
			Name: s.def.Name, Type: s.def.Type, Endpoint: origin(s.def.URL), State: string(s.st.Health),
			CheckInterval: s.def.CheckInterval.String(), Timeout: s.def.Timeout.String(),
			Since: s.st.Since, LastCheck: s.st.LastCheck, LastFailure: string(s.st.LastFailure),
			ConsecutiveFailures: s.st.ConsecutiveFailures, ConsecutiveSuccesses: s.st.ConsecutiveSuccesses,
			IncidentID: s.st.IncidentID, Checks: s.st.Checks, Failures: s.st.Failures,
			LastDurationMS: s.lastDuration.Milliseconds(), LastSlotWaitMS: s.lastSlotWait.Milliseconds(),
			WaitingSince: s.waitingSince, Stalled: s.stalled, BlockedReason: s.blocked,
			Phase: s.phase, Disabled: disabledBy(s),
		})
	}
	if name != "" && len(out.Services) == 0 {
		return api.SupervisorStatusResult{}, &api.Error{Code: api.CodeBadRequest, Message: fmt.Sprintf("no service named %q", name)}
	}
	return out, nil
}

// processStatus is the status of a process service at now; the caller
// holds in.mu.
func processStatus(s *service, now time.Time) api.ServiceStatus {
	r := s.def.Process.Recovery
	b := trim(s.st.budget(), now, r)
	out := api.ServiceStatus{Name: s.def.Name, Type: s.def.Type, State: string(s.st.Health),
		Since: s.st.Since, LastFailure: string(s.st.LastFailure), IncidentID: s.st.IncidentID,
		Failures: s.st.Failures, Phase: s.phase, Pid: s.pid, StartedAt: s.startedAt,
		Starts: s.st.Starts, OutputLost: s.outputLost,
		Restarts: s.st.Restarts, Attempts: len(b.Attempts), MaxAttempts: r.MaxAttempts,
		NextAttemptAt: s.nextAttemptAt, ExhaustedAt: b.ExhaustedAt, ResumeAt: s.resumeAt, BlockedReason: s.blocked,
		Disabled: disabledBy(s)}
	if s.st.Stopped {
		out.StoppedBy = string(gateOperator)
	}
	if out.ResumeAt.IsZero() && !b.ExhaustedAt.IsZero() {
		out.ResumeAt = resumeAt(b.ExhaustedAt, r) // zero with a cooldown of 0: until a reset
	}
	if e := s.st.LastExit; e != nil {
		out.LastExit = &api.ExitStatus{Code: e.Code, Signal: e.Signal, At: e.At}
	}
	return out
}

// disabledBy says who took s out of supervision, if anyone: its
// configuration wins (D-020).
func disabledBy(s *service) string {
	switch {
	case s.def.Disabled:
		return string(gateConfig)
	case s.st.Disabled:
		return string(gateOperator)
	}
	return ""
}

// runningProcesses counts the processes the instance owns now; the caller
// holds in.mu. Their group records are not pending cleanups.
func (in *instance) runningProcesses() int {
	n := 0
	for _, s := range in.services {
		if _, ok := in.groups[s.def.Name]; ok && s.proc != nil {
			n++
		}
	}
	return n
}
