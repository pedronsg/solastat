package main

import "sync/atomic"

// defaultTimezone is Europe/Lisbon — this app's original, hardcoded
// behavior before the setting existed, so an existing install that never
// revisits the wizard/Settings keeps behaving exactly as before.
const defaultTimezone = "Europe/Lisbon"

// systemZone is the device's own time zone, from a system that has one to
// set (read by the app's extension: Device.WatchTimezone); unset on an
// older one, and in the default build, whose SDK cannot read it. Safe for
// concurrent use: it is read on every request.
type systemZone struct {
	name atomic.Pointer[string]
}

// get is the system's time zone, and whether the system has one.
func (z *systemZone) get() (string, bool) {
	if p := z.name.Load(); p != nil {
		return *p, true
	}
	return "", false
}

func (z *systemZone) set(name string) {
	z.name.Store(&name)
}

// Get and Set are get and set for the app's extension (extapi.Zone).
func (z *systemZone) Get() (string, bool) { return z.get() }
func (z *systemZone) Set(name string)     { z.set(name) }

// appTimezone is the time zone the app goes by: the system's when it has
// one (SolaStat's own setting is then neither shown nor used), else the
// one set in SolaStat, else defaultTimezone.
func appTimezone(z *systemZone, setting string) string {
	if tz, ok := z.get(); ok {
		return tz
	}
	if setting != "" {
		return setting
	}
	return defaultTimezone
}
