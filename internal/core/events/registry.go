// Package events validates the events modules emit and delivers them to
// the core's consumers (state, notifications, audit) through an in-process
// bus (ADR-0002 as amended by ADR-0015).
//
// Every event type belongs to one module and must be registered before it
// is published: there are no free-form event types. The bus is for
// observation and notification; enforcement records are written
// synchronously by the audit log, never only through the bus.
package events

import (
	"fmt"
	"regexp"
	"sync"

	"github.com/sentinel-watchdog/sentinel-watchdog/pkg/model"
)

var (
	// moduleNameRe matches module names (internal/core/module uses the same format).
	moduleNameRe = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)
	// typeNameRe matches event type and source type names.
	typeNameRe = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)
)

// TypeSpec declares an event type and the severity it has when the emitter
// does not set one.
type TypeSpec struct {
	Type     model.EventType
	Severity model.Severity
}

// Registry records which module emits which event types. It is safe for
// concurrent use: modules register when they start while the bus reads.
type Registry struct {
	mu    sync.RWMutex
	types map[model.EventType]registered
}

type registered struct {
	module   string
	severity model.Severity
}

// NewRegistry returns a registry that already holds the core's event types.
func NewRegistry() *Registry {
	r := &Registry{types: map[model.EventType]registered{}}
	r.types[model.EventAuditIntegrityBroken] = registered{model.ModuleCore, model.SeverityCritical}
	r.types[model.EventAuditWriteFailed] = registered{model.ModuleCore, model.SeverityCritical}
	r.types[model.EventConfigReloadFailed] = registered{model.ModuleCore, model.SeverityError}
	r.types[model.EventConfigReloaded] = registered{model.ModuleCore, model.SeverityInfo}
	r.types[model.EventConfigurationError] = registered{model.ModuleCore, model.SeverityError}
	r.types[model.EventDaemonError] = registered{model.ModuleCore, model.SeverityError}
	return r
}

// Register declares the event types module emits. A type already
// registered by another module, or by the same module with another
// default severity, is an error: an event type has exactly one owner. The
// same declaration again is accepted, so a module restarted by a reload
// can register its types in Start. The declarations of one call are
// checked against each other too, and a rejected call registers nothing.
func (r *Registry) Register(module string, specs ...TypeSpec) error {
	if !moduleNameRe.MatchString(module) {
		return fmt.Errorf("events: invalid module name %q", module)
	}
	batch := make(map[model.EventType]model.Severity, len(specs))
	for _, s := range specs {
		if !typeNameRe.MatchString(string(s.Type)) {
			return fmt.Errorf("events: invalid event type %q", s.Type)
		}
		if s.Severity.Rank() == 0 {
			return fmt.Errorf("events: event type %q has invalid default severity %q", s.Type, s.Severity)
		}
		if sev, dup := batch[s.Type]; dup && sev != s.Severity {
			return fmt.Errorf("events: event type %q declared twice with severities %q and %q", s.Type, sev, s.Severity)
		}
		batch[s.Type] = s.Severity
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, s := range specs {
		if owner, dup := r.types[s.Type]; dup && (owner.module != module || owner.severity != s.Severity) {
			return fmt.Errorf("events: event type %q already registered by module %q", s.Type, owner.module)
		}
	}
	for _, s := range specs {
		r.types[s.Type] = registered{module, s.Severity}
	}
	return nil
}

// lookup returns the owner and default severity of t.
func (r *Registry) lookup(t model.EventType) (registered, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	reg, ok := r.types[t]
	return reg, ok
}
