package main

import (
	"testing"

	"solastat/internal/solar"
)

func TestFaultNames(t *testing.T) {
	faults := []solar.ActiveFault{
		{ID: "103.17", Group: "Device fault", Name: "AC Over-current failure", Code: "F18"},
		{ID: "322.3", Group: "Battery fault"},
		{ID: "14344=65537", Group: "Device alarm", Code: "65537"},
		{ID: "103.6", Group: "Device fault", Name: "DC/DC Soft Start failure"},
		{ID: "103.9", Group: "Device fault", Name: "Auxiliary power supply failure"},
	}
	want := "F18 · AC Over-current failure, Battery fault 3, Device alarm 65537, +2"
	if got := faultNames(faults); got != want {
		t.Errorf("faultNames = %q, want %q", got, want)
	}
}

func TestNewRegisterFaults(t *testing.T) {
	fan := solar.ActiveFault{ID: "101.1", Name: "Fan failure", Source: solar.FaultFromRegisters}
	ac := solar.ActiveFault{ID: "103.17", Name: "AC Over-current failure", Source: solar.FaultFromRegisters}
	state := solar.ActiveFault{ID: "run_state=4", Source: solar.FaultFromState}

	fresh, seen := newRegisterFaults(nil, []solar.ActiveFault{fan, state})
	if len(fresh) != 1 || fresh[0].ID != "101.1" {
		t.Fatalf("first cycle fresh = %+v, want the fan (the state logs on its own)", fresh)
	}
	// The fan again and the AC fault on top: only the AC fault is news.
	fresh, seen = newRegisterFaults(seen, []solar.ActiveFault{fan, ac, state})
	if len(fresh) != 1 || fresh[0].ID != "103.17" {
		t.Fatalf("second cycle fresh = %+v, want the AC fault", fresh)
	}
	// Cleared, then the fan again: news again.
	_, seen = newRegisterFaults(seen, nil)
	if fresh, _ = newRegisterFaults(seen, []solar.ActiveFault{fan}); len(fresh) != 1 {
		t.Fatalf("fan after clearing: fresh = %+v, want it", fresh)
	}
}
