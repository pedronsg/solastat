package main

import "testing"

// The system's time zone, when it has one, over SolaStat's own setting;
// that setting over the default.
func TestAppTimezone(t *testing.T) {
	var z systemZone
	if got := appTimezone(&z, ""); got != defaultTimezone {
		t.Errorf("nothing set: %q, want %q", got, defaultTimezone)
	}
	if got := appTimezone(&z, "Europe/Madrid"); got != "Europe/Madrid" {
		t.Errorf("set in SolaStat: %q, want Europe/Madrid", got)
	}
	z.set("Europe/Paris")
	if got := appTimezone(&z, "Europe/Madrid"); got != "Europe/Paris" {
		t.Errorf("the system's and SolaStat's: %q, want the system's Europe/Paris", got)
	}
}
