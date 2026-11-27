package setupmode

import "time"

// ledMode is what the user LED is currently telling the user. Modelled on
// Sonoff/Shelly: steady means all good, a calm even blink means setup mode,
// and problems are short flashes after a long pause, counted (1 = no Wi-Fi,
// 2 = inverter), so they are easy to tell apart at a glance. On the current
// board the LED sits on a plain GPIO, so every pattern is an on/off sequence
// driven from the poll loop.
type ledMode int

const (
	ledOff        ledMode = iota
	ledOn                 // all good: Wi-Fi up and the inverter answering
	ledNoWiFi             // no Wi-Fi: 1 flash every 3 s
	ledCommError          // Wi-Fi up, inverter not answering (or not set up yet): 2 flashes every 3 s
	ledAP                 // setup (AP) mode: slow even blink, 1 s on / 1 s off
	ledHeld               // button held, long press not reached yet: steady on
	ledReleaseNow         // long press reached: fast blink, "let go"
)

type ledStep struct {
	on bool
	d  time.Duration
}

const flash = 200 * time.Millisecond

var ledPatterns = map[ledMode][]ledStep{
	ledNoWiFi:     {{true, flash}, {false, 3*time.Second - flash}},
	ledCommError:  {{true, flash}, {false, 300 * time.Millisecond}, {true, flash}, {false, 3*time.Second - 2*flash - 300*time.Millisecond}},
	ledAP:         {{true, time.Second}, {false, time.Second}},
	ledReleaseNow: {{true, 100 * time.Millisecond}, {false, 100 * time.Millisecond}},
}

// ledLit reports whether the LED should be on, elapsed time after mode
// started.
func ledLit(mode ledMode, elapsed time.Duration) bool {
	switch mode {
	case ledOff:
		return false
	case ledOn, ledHeld:
		return true
	}
	steps := ledPatterns[mode]
	var cycle time.Duration
	for _, s := range steps {
		cycle += s.d
	}
	if cycle <= 0 {
		return false
	}
	t := elapsed % cycle
	for _, s := range steps {
		if t < s.d {
			return s.on
		}
		t -= s.d
	}
	return false
}

// ledNextEdge is how long until the pattern for mode next changes level,
// elapsed time after mode started; 0 for steady modes.
func ledNextEdge(mode ledMode, elapsed time.Duration) time.Duration {
	steps := ledPatterns[mode]
	var cycle time.Duration
	for _, st := range steps {
		cycle += st.d
	}
	if cycle <= 0 {
		return 0
	}
	t := elapsed % cycle
	for _, st := range steps {
		if t < st.d {
			return st.d - t
		}
		t -= st.d
	}
	return 0
}
