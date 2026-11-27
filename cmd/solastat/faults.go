package main

import (
	"fmt"
	"strings"

	"solastat/internal/solar"
)

// maxNamedFaults is how many faults one log entry names before "+N more".
const maxNamedFaults = 3

// faultName is how the log and notifications name one fault: the vendor's
// code before its name ("F18 · AC Over-current failure"), whichever of the two
// it has, or its group and number when it has neither ("Battery fault 3").
func faultName(f solar.ActiveFault) string {
	switch {
	case f.Code != "" && f.Name != "":
		return f.Code + " · " + f.Name
	case f.Name != "":
		return f.Name
	case f.Code != "":
		return f.Group + " " + f.Code
	}
	n := f.ID
	if i := strings.LastIndexAny(n, ".="); i >= 0 {
		n = n[i+1:]
	}
	return f.Group + " " + n
}

// faultNames joins the names of faults for one log entry.
func faultNames(faults []solar.ActiveFault) string {
	names := make([]string, 0, maxNamedFaults)
	for i, f := range faults {
		if i == maxNamedFaults {
			names = append(names, fmt.Sprintf("+%d", len(faults)-maxNamedFaults))
			break
		}
		names = append(names, faultName(f))
	}
	return strings.Join(names, ", ")
}

// newRegisterFaults returns the faults from the profile's fault registers
// that weren't active last cycle (seen), and the IDs active now — the next
// call's seen. A second fault appearing while the first is still active is
// news too; the status code and state already have their own log entries.
func newRegisterFaults(seen map[string]bool, faults []solar.ActiveFault) (fresh []solar.ActiveFault, now map[string]bool) {
	now = make(map[string]bool, len(faults))
	for _, f := range faults {
		now[f.ID] = true
		if f.Source == solar.FaultFromRegisters && !seen[f.ID] {
			fresh = append(fresh, f)
		}
	}
	return fresh, now
}
