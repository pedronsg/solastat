package setupmode

import "time"

// Button timing. A long press is what switches setup (AP) mode on or off;
// the action happens on release, once the LED has told the user they can
// let go. Holding far longer than that is treated as something leaning on
// the button and does nothing.
const (
	shortPressMax   = time.Second
	longPressHold   = 5 * time.Second
	longPressCancel = 15 * time.Second

	// debounceSamples is how many consecutive samples must agree before the
	// button is considered pressed or released. Once the first low sample is
	// seen the loop samples every pollActive, so a press has to last about
	// 200 ms to count: noise or a glitch on the line never does.
	debounceSamples = 3
)

type buttonEvent int

const (
	evNone       buttonEvent = iota
	evShortPress             // released before shortPressMax
	evLongReady              // still held, just crossed longPressHold (LED: "release now")
	evLongPress              // released after longPressHold, before longPressCancel
	evCancelled              // still held, just crossed longPressCancel
)

// buttonTracker turns raw pressed/released samples into press events. It
// only depends on the times it is given, so it can be tested without a
// real clock or GPIO.
type buttonTracker struct {
	pressed   bool // debounced state
	candidate bool // state the last differing samples point to
	count     int  // how many consecutive samples agree with candidate
	since     time.Time
	ready     bool          // evLongReady already fired for this press
	cancelled bool          // evCancelled already fired for this press
	lastHeld  time.Duration // how long the last completed press lasted (for logging)
}

func (b *buttonTracker) sample(pressed bool, now time.Time) buttonEvent {
	if pressed == b.pressed {
		b.count = 0
		return b.whileHeld(now)
	}
	if pressed != b.candidate || b.count == 0 {
		b.candidate = pressed
		b.count = 0
	}
	b.count++
	if b.count < debounceSamples {
		return b.whileHeld(now)
	}

	b.pressed = pressed
	b.count = 0
	if pressed {
		b.since = now
		b.ready = false
		b.cancelled = false
		b.lastHeld = 0
		return evNone
	}

	held := now.Sub(b.since)
	b.lastHeld = held
	switch {
	case b.cancelled:
		return evNone
	case held >= longPressHold:
		return evLongPress
	case held < shortPressMax:
		return evShortPress
	}
	return evNone
}

func (b *buttonTracker) whileHeld(now time.Time) buttonEvent {
	if !b.pressed {
		return evNone
	}
	held := now.Sub(b.since)
	if !b.cancelled && held >= longPressCancel {
		b.cancelled = true
		return evCancelled
	}
	if !b.ready && held >= longPressHold {
		b.ready = true
		return evLongReady
	}
	return evNone
}

// readyToRelease reports whether the button is being held long enough that
// letting go now would trigger a long press.
func (b *buttonTracker) readyToRelease() bool {
	return b.pressed && b.ready && !b.cancelled
}
