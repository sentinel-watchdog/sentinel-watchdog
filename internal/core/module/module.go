// Package module defines what a Sentinel module is and keeps the registry
// of the modules built into a binary (ADR-0015).
//
// A module is a feature area enabled by the central configuration file
// (supervisor, firewall, ...). It lives in internal/modules/<name>, never
// imports another module, and is wired by internal/daemon through an
// explicit Registry: there is no init() self-registration.
//
// The contract grows with the phases that need it: the runtime services a
// module receives at Start (events, state, notifications, audit), its
// status and its CLI commands arrive in Phases 2b and 2c.
package module

import (
	"context"

	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/config"
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
type Configured interface {
	// Start launches the module's work and returns once it is running.
	Start(ctx context.Context) error
	// Stop ends the module's work; ctx bounds how long it may take.
	Stop(ctx context.Context) error
}

// Factory creates a fresh, unconfigured module.
type Factory func() Module
