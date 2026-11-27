package main

// commErrorAfterCycles is how many poll cycles in a row must fail before
// the inverter counts as unreachable (about 15 s at the usual 5 s poll).
// A single failed cycle — one dropped packet on Wi-Fi, one connection a
// Solarman datalogger refused — recovers on the next cycle, and shouldn't
// raise a "Communication error" notification or blink the LED.
const commErrorAfterCycles = 3

// commHealth debounces poll results into one "is the inverter answering"
// state, shared by the activity log/notifications, the device's LED and MQTT.
// Not safe for concurrent use; callers hold their own lock.
type commHealth struct {
	ok      bool // inverter answering; false until the first good cycle
	failed  int  // consecutive failed cycles
	lastErr string
}

// observe records one poll cycle and reports whether ok changed.
func (h *commHealth) observe(errText string, readings int) (changed bool) {
	was := h.ok
	switch {
	case errText == "" && readings > 0:
		h.failed = 0
		h.ok = true
	case errText != "":
		h.failed++
		h.lastErr = errText
		if h.failed >= commErrorAfterCycles {
			h.ok = false
		}
	}
	return h.ok != was
}

// down reports a real outage: commErrorAfterCycles or more failed cycles in
// a row. Unlike !ok, this is false before the first cycle, so a device still
// starting up (or with no inverter set up) doesn't count as an outage.
func (h *commHealth) down() bool {
	return h.failed >= commErrorAfterCycles
}
