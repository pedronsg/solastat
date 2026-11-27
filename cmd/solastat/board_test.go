package main

import (
	"strings"

	"solastat/internal/extapi"
	"testing"
)

func TestLocalHost(t *testing.T) {
	for name, want := range map[string]string{
		"orbit-2ce8":       "orbit-2ce8.local",
		"orbit-2ce8.local": "orbit-2ce8.local",
		"Unknown":          "",
		"":                 "",
	} {
		if got := localHost(name); got != want {
			t.Errorf("localHost(%q) = %q, want %q", name, got, want)
		}
	}
}

func TestEdition(t *testing.T) {
	for e, want := range map[string]string{"ACME": "ACME", "": "DIY"} {
		if got := edition(extapi.Unit{Edition: e}); got != want {
			t.Errorf("edition(%q) = %q, want %q", e, got, want)
		}
	}
}

func TestValidDeviceName(t *testing.T) {
	for name, ok := range map[string]bool{
		"SolaStat garagem":      true,
		"Casa da avó ☀":         true,
		"x":                     true,
		strings.Repeat("é", 63): true,
		"":                      false,
		strings.Repeat("a", 64): false,
		"two\nlines":            false,
		"tab\there":             false,
		"bad \xff":              false,
	} {
		if err := validDeviceName(name); (err == nil) != ok {
			t.Errorf("validDeviceName(%q) = %v, want ok %v", name, err, ok)
		}
	}
}
