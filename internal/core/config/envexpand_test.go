package config

import (
	"runtime"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

func TestExpandString(t *testing.T) {
	vars := env(map[string]string{
		"TOKEN": "s3cr3t",
		"HOST":  "example.org",
		"EMPTY": "",
		"NEST":  "${TOKEN}",
	})
	tests := []struct {
		in, want, wantErr string
	}{
		{"plain", "plain", ""},
		{"Bearer ${TOKEN}", "Bearer s3cr3t", ""},
		{"https://${HOST}/${TOKEN}", "https://example.org/s3cr3t", ""},
		{"${EMPTY}", "", ""},
		{"cost $5", "cost $5", ""},
		{"$HOST", "$HOST", ""},
		{"$${TOKEN}", "${TOKEN}", ""},
		{"${NEST}", "${TOKEN}", ""}, // not recursive
		{"${MISSING}", "", `"MISSING" is not set`},
		{"${1BAD}", "", "invalid variable name"},
		{"${}", "", "invalid variable name"},
		{"${TOKEN", "", "unterminated"},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, err := expandString(tt.in, vars, maxExpansionGrowth)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestExpandStringReportsAllMissing(t *testing.T) {
	_, err := expandString("${A} ${B}", env(nil), maxExpansionGrowth)
	if err == nil || !strings.Contains(err.Error(), `"A"`) || !strings.Contains(err.Error(), `"B"`) {
		t.Fatalf("expected both variables reported, got %v", err)
	}
}

// The expansion budget is charged while a value is built: a short value
// with many references to a large variable must fail before allocating
// its full expansion.
func TestExpandNodeChargesBudgetBeforeAllocating(t *testing.T) {
	big := strings.Repeat("x", 1<<20)
	n := &yaml.Node{Kind: yaml.ScalarNode, Value: strings.Repeat("${BIG}", 256)}
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	_, err := expandNode(n, env(map[string]string{"BIG": big}))
	runtime.ReadMemStats(&after)
	if err == nil || !strings.Contains(err.Error(), "add more than") {
		t.Fatalf("err = %v, want the expansion budget error", err)
	}
	if grew := after.TotalAlloc - before.TotalAlloc; grew > 4*maxExpansionGrowth {
		t.Errorf("allocated %d MiB before failing", grew>>20)
	}
}
