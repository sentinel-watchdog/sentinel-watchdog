package events

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/sentinel-watchdog/sentinel-watchdog/pkg/model"
)

// testRegistry registers the "test" module's event types.
func testRegistry(t *testing.T) *Registry {
	t.Helper()
	r := NewRegistry()
	if err := r.Register("test", TypeSpec{"thing_failed", model.SeverityError}, TypeSpec{"thing_ok", model.SeverityInfo}); err != nil {
		t.Fatal(err)
	}
	return r
}

// event returns a valid event of the test module.
func event() model.Event {
	return model.Event{Module: "test", Source: "nginx", SourceType: "systemd", Type: "thing_failed", Message: "down"}
}

func TestNormalizeRejectsEmitterMistakes(t *testing.T) {
	tests := []struct {
		name   string
		change func(*model.Event)
		want   string
	}{
		{"unregistered type", func(e *model.Event) { e.Type = "nope" }, "not registered"},
		{"wrong module", func(e *model.Event) { e.Module = "other" }, `belongs to module "test"`},
		{"invalid severity", func(e *model.Event) { e.Severity = "fatal" }, "invalid severity"},
		{"invalid source type", func(e *model.Event) { e.SourceType = "Systemd!" }, "invalid source_type"},
		{"empty source", func(e *model.Event) { e.Source = "\x00\x01" }, "source is empty"},
		{"metadata key", func(e *model.Event) { e.Metadata = map[string]string{"Bad Key": "v"} }, "invalid metadata key"},
		{"attribute key", func(e *model.Event) { e.Attributes = map[string]any{"a/b": 1} }, "invalid attribute key"},
		{"reserved attribute key", func(e *model.Event) { e.Attributes = map[string]any{truncatedKey: false} }, "invalid attribute key"},
		{"attribute type", func(e *model.Event) { e.Attributes = map[string]any{"x": struct{}{}} }, "unsupported type"},
		{"two nesting levels", func(e *model.Event) {
			e.Attributes = map[string]any{"a": map[string]any{"b": map[string]any{"c": 1}}}
		}, "one level of nesting"},
		{"NaN", func(e *model.Event) { e.Attributes = map[string]any{"x": math.NaN()} }, "NaN"},
		{"long list", func(e *model.Event) { e.Attributes = map[string]any{"x": make([]string, maxListItems+1)} }, "items"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := event()
			tt.change(&e)
			_, err := normalize(e, testRegistry(t), DefaultMaxEventBytes)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("err = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestNormalizeCleansContent(t *testing.T) {
	e := event()
	e.Severity = ""
	e.Message = "a\x1b[31mb\nc\xff" + strings.Repeat("é", maxMessageBytes)
	e.Metadata = map[string]string{"label": "x\ty"}
	e.Attributes = map[string]any{
		"exit_code": 3, "scenarios": []string{"ssh\r\nbf"}, "nested": map[string]any{"ok": true},
	}
	got, err := normalize(e, testRegistry(t), DefaultMaxEventBytes)
	if err != nil {
		t.Fatal(err)
	}
	if got.Severity != model.SeverityError {
		t.Errorf("default severity not applied: %q", got.Severity)
	}
	if !strings.HasPrefix(got.Message, "a[31mbc�") || len(got.Message) > maxMessageBytes || !utf8.ValidString(got.Message) {
		t.Errorf("message not cleaned: %q", got.Message[:20])
	}
	if got.Metadata["label"] != "xy" || got.Attributes["scenarios"].([]string)[0] != "sshbf" {
		t.Errorf("values not cleaned: %v %v", got.Metadata, got.Attributes)
	}
}

// The emitter's maps are copied: changing them after Publish cannot change
// what consumers see.
func TestNormalizeCopiesMaps(t *testing.T) {
	e := event()
	e.Attributes = map[string]any{"n": 1}
	e.Metadata = map[string]string{"k": "v"}
	got, err := normalize(e, testRegistry(t), DefaultMaxEventBytes)
	if err != nil {
		t.Fatal(err)
	}
	e.Attributes["n"], e.Metadata["k"] = 2, "changed"
	if got.Attributes["n"] != 1 || got.Metadata["k"] != "v" {
		t.Errorf("normalized event shares the emitter's maps: %v %v", got.Attributes, got.Metadata)
	}
}

func TestNormalizeFitsTheSizeCap(t *testing.T) {
	big := strings.Repeat("x", maxValueBytes)
	escaped := strings.Repeat("<", maxValueBytes) // six bytes each in JSON
	tests := []struct {
		name                 string
		change               func(*model.Event)
		attributes, metadata bool // kept?
		messageShortened     bool
	}{
		{"small event untouched", func(*model.Event) {}, true, true, false},
		{"large attributes dropped", func(e *model.Event) {
			e.Attributes = map[string]any{}
			for i := range maxMapEntries {
				e.Attributes[strings.Repeat("a", i+1)] = big
			}
		}, false, true, false},
		{"escaped metadata dropped", func(e *model.Event) {
			e.Metadata = map[string]string{}
			for i := range maxMapEntries {
				e.Metadata[strings.Repeat("m", i+1)] = escaped
			}
		}, false, false, false},
		{"escaped message shortened", func(e *model.Event) {
			e.Message = strings.Repeat("<", maxMessageBytes)
			e.Source = strings.Repeat("<", maxIdentifierBytes)
			e.State, e.PreviousState, e.CorrelationID = e.Source, e.Source, e.Source
		}, false, false, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := event()
			e.Attributes = map[string]any{"n": 1}
			e.Metadata = map[string]string{"k": "v"}
			tt.change(&e)
			got, err := normalize(e, testRegistry(t), minMaxEventBytes)
			if err != nil {
				t.Fatal(err)
			}
			data, _ := json.Marshal(got)
			if len(data) > minMaxEventBytes {
				t.Fatalf("encoded event has %d bytes, cap %d", len(data), minMaxEventBytes)
			}
			truncated := got.Attributes[truncatedKey] == true
			if truncated == tt.attributes || (got.Metadata != nil) != tt.metadata ||
				(len(got.Message) < len(e.Message)) != tt.messageShortened {
				t.Errorf("attributes kept %v, metadata kept %v, message %d/%d bytes",
					!truncated, got.Metadata != nil, len(got.Message), len(e.Message))
			}
		})
	}
}

func TestCleanCutsOnCharacterBoundaries(t *testing.T) {
	s := clean(strings.Repeat("é", 10), 5) // two bytes each
	if s != "éé" || !utf8.ValidString(s) {
		t.Errorf("clean = %q", s)
	}
}

// FuzzNormalize: whatever text an external system puts in an event, the
// result is clean UTF-8 within its limits, and the encoding fits the cap.
func FuzzNormalize(f *testing.F) {
	f.Add("down", "nginx", "value")
	f.Add("a\x1b[31m\n", "\xff\xfe", strings.Repeat("<", 2000))
	f.Fuzz(func(t *testing.T, message, source, value string) {
		r := NewRegistry()
		if err := r.Register("test", TypeSpec{"thing_failed", model.SeverityError}); err != nil {
			t.Fatal(err)
		}
		e := event()
		e.Message, e.Source = message, "s"+source
		e.Metadata = map[string]string{"k": value}
		e.Attributes = map[string]any{"v": value, "l": []string{value, value}}
		got, err := normalize(e, r, DefaultMaxEventBytes)
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range []string{got.Message, got.Source, got.Metadata["k"]} {
			if !utf8.ValidString(s) || strings.ContainsFunc(s, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
				t.Fatalf("unclean string %q", s)
			}
		}
		if data, _ := json.Marshal(got); len(data) > DefaultMaxEventBytes {
			t.Fatalf("encoded event has %d bytes", len(data))
		}
	})
}
