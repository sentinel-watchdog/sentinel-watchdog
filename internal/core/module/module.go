// Package module defines what a Sentinel module is and keeps the registry
// of the modules built into a binary (ADR-0015).
//
// A module is a feature area enabled by the central configuration file
// (supervisor, firewall, ...). It lives in internal/modules/<name>, never
// imports another module, and is wired by internal/daemon through an
// explicit Registry: there is no init() self-registration.
//
// The contract grows with the phases that need it: a module receives its
// runtime services at Start (Phase 2c) and adds control-socket commands
// (Commander, Handler; Phase 3a); status is added when it has a consumer
// (R-022).
package module

import (
	"context"
	"encoding/json/jsontext"
	"log/slog"

	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/clock"
	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/config"
	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/events"
	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/state"
	"github.com/sentinel-watchdog/sentinel-watchdog/pkg/api"
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
	// Secrets are the secret values of the configuration the module was
	// configured from (Config.Secrets), to mask in text that comes from
	// outside the module, such as the output of a program it runs, with
	// redact.Text.
	Secrets []string
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

// Warner is implemented by a configured module whose configuration is valid
// but risky, such as TLS verification turned off (D-019). Configure can
// return warnings only this way, or in a *config.ValidationError when it
// also fails. The registry reports them with the loader's warnings.
//
// Problems and warnings name keys and lines, never the configured values:
// the registry masks the configuration's known secrets as a safety net
// only, and like Config.RedactText it leaves values shorter than four
// bytes alone, which would otherwise shred unrelated words (D-080).
type Warner interface {
	Warnings() []config.Problem
}

// Commander is implemented by a module that adds commands to the control
// socket, such as supervisor.status. The commands are declared by the
// unconfigured module, so they and their tiers are fixed for the life of
// the socket and never depend on the configuration (D-080). Each name is
// "<module>.<command>"; the registry checks names and tiers at Register.
type Commander interface {
	Commands() []api.Command
}

// Handler is implemented by the configured module of a Commander: it runs
// one of the declared commands. The daemon calls it only between a
// successful Start and Stop, and Stop waits for the calls in flight, so a
// module never handles a command while it starts or stops. Calls may be
// concurrent with each other and with the module's work. ctx ends with the
// client's exchange. args is a JSON object or empty.
//
// The result is encoded as JSON for the client. Results and error messages
// reach every peer of the command's tier: like events, they never carry
// secrets (D-080 shows URLs as scheme, host and port only).
type Handler interface {
	Handle(ctx context.Context, command string, args jsontext.Value) (any, *api.Error)
}

// Factory creates a fresh, unconfigured module.
type Factory func() Module
