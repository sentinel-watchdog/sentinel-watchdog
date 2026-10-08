package events

import (
	"strings"
	"testing"

	"github.com/sentinel-watchdog/sentinel-watchdog/pkg/model"
)

func TestRegistry(t *testing.T) {
	r := NewRegistry()
	if reg, ok := r.lookup(model.EventDaemonError); !ok || reg.module != model.ModuleCore {
		t.Fatalf("core event types are not pre-registered: %+v %v", reg, ok)
	}
	if err := r.Register("supervisor", TypeSpec{"service_failed", model.SeverityError}); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name   string
		module string
		spec   TypeSpec
		want   string
	}{
		{"type owned by another module", "firewall", TypeSpec{"service_failed", model.SeverityError}, `already registered by module "supervisor"`},
		{"core type", "supervisor", TypeSpec{model.EventDaemonError, model.SeverityError}, `already registered by module "core"`},
		{"invalid module", "Bad", TypeSpec{"x", model.SeverityInfo}, "invalid module name"},
		{"invalid type", "supervisor", TypeSpec{"Service-Failed", model.SeverityInfo}, "invalid event type"},
		{"invalid severity", "supervisor", TypeSpec{"service_flapping", "fatal"}, "invalid default severity"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := r.Register(tt.module, tt.spec)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("err = %v, want %q", err, tt.want)
			}
		})
	}
	// A failed registration registers nothing.
	if err := r.Register("supervisor", TypeSpec{"a_ok", model.SeverityInfo}, TypeSpec{"service_failed", model.SeverityInfo}); err == nil {
		t.Fatal("duplicate in a batch accepted")
	}
	if _, ok := r.lookup("a_ok"); ok {
		t.Error("a failed batch registered some of its types")
	}
}
