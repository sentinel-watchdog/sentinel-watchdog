package supervisor

import (
	"fmt"
	"net/url"
	"time"

	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/events"
	"github.com/sentinel-watchdog/sentinel-watchdog/pkg/model"
)

// The supervisor's event types (ADR-0015 §5), registered at Start.
const (
	EventServiceFailed    model.EventType = "service_failed"
	EventServiceRecovered model.EventType = "service_recovered"
)

var eventSpecs = []events.TypeSpec{
	{Type: EventServiceFailed, Severity: model.SeverityError},
	{Type: EventServiceRecovered, Severity: model.SeverityInfo},
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
