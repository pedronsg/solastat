package solar

import (
	"testing"

	"solastat/internal/profile"
)

// A magnitude takes the sign of the reading it names (Solis' battery
// current, its battery power's), and is left as read without one.
func TestSignReadings(t *testing.T) {
	regs := []profile.Register{
		{Name: "battery_power"},
		{Name: "battery_current", SignOf: "battery_power"},
		{Name: "pv1_total_power_p_i", SignOf: "missing"},
	}
	for _, c := range []struct {
		power, current, want float64
	}{
		{-1500, 30, -30}, // charging
		{1500, 30, 30},   // discharging
		{1500, -30, 30},  // already signed, the other way
		{0, 0.4, 0.4},    // idle: as read
	} {
		readings := map[string]Reading{
			"battery_power":       {Value: c.power},
			"battery_current":     {Value: c.current},
			"pv1_total_power_p_i": {Value: 7},
		}
		signReadings(readings, regs)
		if got := readings["battery_current"].Value; got != c.want {
			t.Errorf("power %v, current %v: got %v, want %v", c.power, c.current, got, c.want)
		}
		if got := readings["pv1_total_power_p_i"].Value; got != 7 {
			t.Errorf("a missing sign reading changed the value to %v", got)
		}
	}
}
