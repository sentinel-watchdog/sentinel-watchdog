package config

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// loadAgain rewrites the files in the directory of main and loads them.
func loadAgain(t *testing.T, main string, files map[string]string, vars map[string]string) *Config {
	t.Helper()
	dir := filepath.Dir(main)
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cfg, err := Load(LoadOptions{MainFile: main, Modules: testModules, Lookup: env(vars)})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return cfg
}

const (
	centralAlpha = "version: 1\nmodules:\n  alpha: {enabled: true}\n"
	alphaFile    = "version: 1\nsettings:\n  a: 1\n  b: \"${B}\"\n"
)

// The digest is the SHA-256 of the files as read, so an operator can check
// it with standard tools: path, length and bytes of each file, in load
// order, before ${VARIABLE} expansion (T-02, D-077).
func TestDigest(t *testing.T) {
	main := writeTree(t, map[string]string{"sentinel.yaml": centralAlpha, "alpha/10-a.yaml": alphaFile})
	vars := map[string]string{"B": "x"}
	cfg := loadAgain(t, main, nil, vars)
	h := sha256.New()
	for _, f := range []struct{ path, body string }{
		{main, centralAlpha}, {filepath.Join(filepath.Dir(main), "alpha", "10-a.yaml"), alphaFile},
	} {
		fmt.Fprintf(h, "%s\x00%d\x00%s", f.path, len(f.body), f.body)
	}
	if want := "sha256:" + hex.EncodeToString(h.Sum(nil)); cfg.Digest != want {
		t.Errorf("Digest = %s, want %s", cfg.Digest, want)
	}
	if again := loadAgain(t, main, nil, map[string]string{"B": "secret-y"}); again.Digest != cfg.Digest {
		t.Error("an environment value changed the digest: it would hash the secret")
	}
	if edited := loadAgain(t, main, map[string]string{"alpha/10-a.yaml": alphaFile + "  c: 3\n"}, vars); edited.Digest == cfg.Digest {
		t.Error("an edited file kept the digest")
	}
}

// ModuleConfig.Equal compares content, not layout: key order and comments
// do not count, values and expanded environment values do.
func TestModuleConfigEqual(t *testing.T) {
	main := writeTree(t, map[string]string{"sentinel.yaml": centralAlpha, "alpha/10-a.yaml": alphaFile})
	vars := map[string]string{"B": "x"}
	base, _ := loadAgain(t, main, nil, vars).Module("alpha")
	tests := []struct {
		name  string
		files map[string]string
		vars  map[string]string
		equal bool
	}{
		{"unchanged", nil, vars, true},
		{"reordered and commented", map[string]string{"alpha/10-a.yaml": "version: 1\n# note\nsettings:\n  b: \"${B}\"   # b\n  a: 1\n"}, vars, true},
		{"value", map[string]string{"alpha/10-a.yaml": "version: 1\nsettings:\n  a: 2\n  b: \"${B}\"\n"}, vars, false},
		{"environment value", nil, map[string]string{"B": "y"}, false},
		{"central block", map[string]string{"sentinel.yaml": "version: 1\nmodules:\n  alpha: {enabled: true, mode: x}\n"}, vars, false},
		{"file added", map[string]string{"alpha/20-b.yaml": "version: 1\nitems: []\n"}, vars, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Start every case from the base files.
			loadAgain(t, main, map[string]string{"sentinel.yaml": centralAlpha, "alpha/10-a.yaml": alphaFile}, vars)
			_ = os.Remove(filepath.Join(filepath.Dir(main), "alpha", "20-b.yaml"))
			got, ok := loadAgain(t, main, tt.files, tt.vars).Module("alpha")
			if !ok {
				t.Fatal("alpha not configured")
			}
			if eq := base.Equal(got); eq != tt.equal {
				t.Errorf("Equal = %v, want %v", eq, tt.equal)
			}
		})
	}
}
