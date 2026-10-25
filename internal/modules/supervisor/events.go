package supervisor

import (
	"fmt"
	"net/url"
	"time"

	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/events"
	"github.com/sentinel-watchdog/sentinel-watchdog/internal/platform/executor"
	"github.com/sentinel-watchdog/sentinel-watchdog/pkg/model"
)

// The supervisor's event types (ADR-0015 §5), registered at Start.
const (
	EventServiceFailed    model.EventType = "service_failed"
	EventServiceRecovered model.EventType = "service_recovered"
	// EventServiceRestarted: a process was launched again by recovery;
	// "restarted" means launched, "recovered" means it ran for
	// stable_after (D-087).
	EventServiceRestarted model.EventType = "service_restarted"
	// EventServiceRecoveryExhausted: the recovery budget is spent; no
	// restart until the cooldown ends.
	EventServiceRecoveryExhausted model.EventType = "service_recovery_exhausted"
)

var eventSpecs = []events.TypeSpec{
	{Type: EventServiceFailed, Severity: model.SeverityError},
	{Type: EventServiceRecovered, Severity: model.SeverityInfo},
	{Type: EventServiceRestarted, Severity: model.SeverityWarning},
	{Type: EventServiceRecoveryExhausted, Severity: model.SeverityCritical},
}

// failedEvent is the service_failed of s; id is the incident's, also its
// correlation id, so that the recovery can name it.
func failedEvent(s *service, id string) model.Event {
	return model.Event{ID: id, CorrelationID: id, Source: s.def.Name, SourceType: model.SourceType(s.def.Type),
		Type: EventServiceFailed, State: string(HealthFailed), PreviousState: string(s.prev),
		Message: fmt.Sprintf("service %s failed: %d consecutive checks failed (%s)",
			s.def.Name, s.st.ConsecutiveFailures, s.st.LastFailure),
		Attributes: map[string]any{
			"failure":              string(s.st.LastFailure),
			"consecutive_failures": s.st.ConsecutiveFailures,
			"endpoint":             origin(s.def.URL),
		}}
}

// recoveredEvent is the service_recovered that closes incident.
func recoveredEvent(s *service, incident string, downtime time.Duration) model.Event {
	return model.Event{CorrelationID: incident, Source: s.def.Name, SourceType: model.SourceType(s.def.Type),
		Type: EventServiceRecovered, State: string(HealthHealthy), PreviousState: string(HealthFailed),
		Message: fmt.Sprintf("service %s recovered after %d successful checks", s.def.Name, s.st.ConsecutiveSuccesses),
		Attributes: map[string]any{
			"consecutive_successes": s.st.ConsecutiveSuccesses,
			"downtime_seconds":      int64(downtime / time.Second),
			"endpoint":              origin(s.def.URL),
		}}
}

// origin is the part of a URL that events, logs and status show: scheme,
// host and port. url.URL.Host holds no user information; the path, the
// query and the fragment may hold a token (D-080 (4)).
func origin(u *url.URL) string {
	if u == nil {
		return ""
	}
	return u.Scheme + "://" + u.Host
}

// processFailedEvent is the service_failed of a process service: a start
// that failed (exit nil) or a process that ended. It names neither the
// program nor its arguments.
func processFailedEvent(s *service, id string, exit *executor.Exit, startup bool) model.Event {
	attrs := map[string]any{"failure": string(s.st.LastFailure)}
	msg := fmt.Sprintf("service %s failed: it could not be started (%s)", s.def.Name, s.st.LastFailure)
	if exit != nil {
		if exit.Signal != "" {
			attrs["signal"] = exit.Signal
		} else {
			attrs["exit_code"] = exit.Code
		}
		attrs["startup"] = startup
		during := ""
		if startup {
			during = " during startup"
		}
		msg = fmt.Sprintf("service %s failed: its process ended with %s%s", s.def.Name, exit, during)
	}
	return model.Event{ID: id, CorrelationID: id, Source: s.def.Name, SourceType: model.SourceType(s.def.Type),
		Type: EventServiceFailed, State: string(HealthFailed), PreviousState: string(s.prev),
		Message: msg, Attributes: attrs}
}

// processRecoveredEvent closes the incident of a process service that has
// run for stable_after.
func processRecoveredEvent(s *service, incident string, downtime time.Duration) model.Event {
	return model.Event{CorrelationID: incident, Source: s.def.Name, SourceType: model.SourceType(s.def.Type),
		Type: EventServiceRecovered, State: string(HealthHealthy), PreviousState: string(HealthFailed),
		Message: fmt.Sprintf("service %s recovered: its process has run for %s", s.def.Name, s.def.Process.Recovery.StableAfter),
		Attributes: map[string]any{
			"downtime_seconds": int64(downtime / time.Second),
			"restarts":         s.st.Restarts,
		}}
}

// restartedEvent reports attempt a of the incident's recovery, launched.
func restartedEvent(s *service, a attempt) model.Event {
	return model.Event{CorrelationID: a.correlation, Source: s.def.Name, SourceType: model.SourceType(s.def.Type),
		Type: EventServiceRestarted, State: string(HealthFailed), PreviousState: string(HealthFailed),
		Message: fmt.Sprintf("service %s restarted (attempt %d of %d)", s.def.Name, a.n, s.def.Process.Recovery.MaxAttempts),
		Attributes: map[string]any{
			"attempt":      a.n,
			"max_attempts": s.def.Process.Recovery.MaxAttempts,
			"trigger":      a.trigger,
		}}
}

// exhaustedEvent reports that the incident's recovery budget is spent;
// resumeAt is zero with a cooldown of 0 (D-013).
func exhaustedEvent(s *service, incident string, resumeAt time.Time) model.Event {
	r := s.def.Process.Recovery
	e := model.Event{CorrelationID: incident, Source: s.def.Name, SourceType: model.SourceType(s.def.Type),
		Type: EventServiceRecoveryExhausted, State: string(HealthFailed), PreviousState: string(HealthFailed),
		Message: fmt.Sprintf("service %s: recovery exhausted after %d attempts in %s; next attempt after the cooldown of %s",
			s.def.Name, r.MaxAttempts, r.Window, r.Cooldown),
		Attributes: map[string]any{
			"max_attempts":     r.MaxAttempts,
			"window_seconds":   int64(r.Window / time.Second),
			"cooldown_seconds": int64(r.Cooldown / time.Second),
		}}
	if resumeAt.IsZero() {
		e.Message = fmt.Sprintf("service %s: recovery exhausted after %d attempts in %s; no attempt until `sentinelctl supervisor reset %s`",
			s.def.Name, r.MaxAttempts, r.Window, s.def.Name)
	} else {
		e.Attributes["resume_at"] = resumeAt.UTC().Format(time.RFC3339)
	}
	return e
}
