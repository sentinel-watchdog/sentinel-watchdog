package config

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"
)

func TestCheckKnownFieldsBoundsAliasExpansion(t *testing.T) {
	// Each level references the previous one ten times: walking the last
	// item naively would visit about ten million nodes.
	src := "- &a [x, x, x, x, x, x, x, x, x, x]\n"
	prev := "a"
	for _, name := range []string{"b", "c", "d", "e", "f", "g"} {
		refs := strings.TrimSuffix(strings.Repeat("*"+prev+", ", 10), ", ")
		src += "- &" + name + " [" + refs + "]\n"
		prev = name
	}
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(src), &doc); err != nil {
		t.Fatal(err)
	}
	type deep [][][][][][][][]string
	start := time.Now()
	err := checkKnownFields(&doc, reflect.TypeFor[deep]())
	if err == nil || !strings.Contains(err.Error(), "too complex") {
		t.Fatalf("err = %v, want a complexity error", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("walk took %s", elapsed)
	}
}
