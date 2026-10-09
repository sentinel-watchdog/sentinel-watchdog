// Package module defines what a Sentinel module is and keeps the registry
// of the modules built into a binary (ADR-0015).
//
// A module is a feature area enabled by the central configuration file
// (supervisor, firewall, ...). It lives in internal/modules/<name>, never
// imports another module, and is wired by internal/daemon through an
// explicit Registry: there is no init() self-registration.
//
// The contract grows with the phases that need it: a module receives its
// runtime services at Start (Phase 2c); audit, status and CLI commands are
// added when they have a consumer (R-022).
package module

import (
	"context"
	"log/slog"

	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/clock"
	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/config"
	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/events"
	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/state"
	"github.com/sentinel-watchdog/sentinel-watchdog/pkg/model"
)

// Module is a feature area of the agent.
type Module interface {
	// Name is the module's configuration key, directory name and CLI
	// namespace, such as "supervisor".
	Name() string
	// Configure validates the module's configuration and returns a module
	// ready to start. It must not start goroutines or change the system:
	// `sentinelctl validate` and configuration reloads call it too.
	//
	// Problems should be returned as a *config.ValidationError so that
	// every finding is reported with its file and line.
	Configure(cfg config.ModuleConfig) (Configured, error)
}

// Configured is a validated module, ready to run.
//
// The daemon recovers panics in Start and Stop and bounds how long they
// may take. It cannot recover panics in goroutines the module starts, nor
// stop them: a module cancels, joins and recovers its own goroutines.
type Configured interface {
	// Start launches the module's work and returns once it is running.
	// ctx is cancelled when the daemon gives up waiting for Start; the
	// module's work must stop when Stop is called, not when ctx ends.
	Start(ctx context.Context, rt Runtime) error
	// Stop ends the module's work; ctx bounds how long it may take.
	Stop(ctx context.Context) error
}

// Runtime holds the services the daemon gives a module at Start. Every
// service is bound to the module: it cannot publish events or open state
// in another module's name.
type Runtime struct {
	Logger *slog.Logger
	Clock  clock.Clock
	// Events registers the module's event types and publishes its events.
	Events *events.Emitter
	// State opens the module's state file; schemaVersion is the version
	// this build of the module writes (state.Dir.Store).
	State func(schemaVersion int) (*state.Store, error)
}

// Router is implemented by a configured module whose events are routed to
// notification channels. The routes belong to the items the module
// configures (D-004, D-072), so only the module can tell where an event
// goes; the daemon asks it for each event. A module without a Router has
// its events logged only. NotificationChannels is called concurrently with
// the module's work and must not block.
type Router interface {
	NotificationChannels(e model.Event) []string
}

// Factory creates a fresh, unconfigured module.
type Factory func() Module
