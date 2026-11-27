package main

import (
	"os"
	"path/filepath"
	"testing"
)

// In a repo checkout the data live in devdata/, never under orb/ (packed
// into the .orb as it is), from the repo root and from cmd/solastat; on a
// device, where there is no orb/, in the working directory.
func TestResolveDataDir(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "cmd/solastat/orb/data/inverters"), 0o755); err != nil {
		t.Fatal(err)
	}
	device := t.TempDir()
	for dir, want := range map[string]string{
		root:                                "cmd/solastat/devdata",
		filepath.Join(root, "cmd/solastat"): "devdata",
		device:                              ".",
	} {
		t.Chdir(dir)
		got, err := resolveDataDir("")
		if err != nil || got != want {
			t.Errorf("from %s: %q, %v; want %q", dir, got, err, want)
		}
	}
	if got, _ := resolveDataDir("/data/x/"); got != "/data/x" {
		t.Errorf("--data-dir: %q", got)
	}
}
