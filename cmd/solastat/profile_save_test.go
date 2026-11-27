package main

import (
	"strings"
	"testing"

	"solastat/internal/profile"
)

func tcpProfile(host string, serial uint32) *profile.Profile {
	return &profile.Profile{
		Name:      "Deye",
		Uart:      profile.UartSettings{BaudRate: 9600, DataBits: 8, StopBits: 1, Parity: "none"},
		Modbus:    profile.ModbusSettings{SlaveAddress: 1, Connection: "tcp", TCPHost: host, TCPSerial: serial},
		Registers: []profile.Register{{Name: "pv_total_power", Address: 1, FunctionCode: 3}},
	}
}

func TestSaveProfileSolarmanNeedsAValidAddress(t *testing.T) {
	dir := t.TempDir()
	refused := map[string]struct {
		p    *profile.Profile
		want string
	}{
		"no IP":             {tcpProfile("", 123), "IP address is empty"},
		"blank IP":          {tcpProfile("   ", 123), "IP address is empty"},
		"three octets":      {tcpProfile("192.168.1", 0), "not a valid IP"},
		"octet over 255":    {tcpProfile("192.168.1.300", 0), "not a valid IP"},
		"space in the name": {tcpProfile("my logger", 0), "not a valid IP"},
		"unknown transport": {func() *profile.Profile { p := tcpProfile("10.0.0.2", 1); p.Modbus.Connection = "udp"; return p }(), "connection must be"},
	}
	for name, c := range refused {
		err := saveProfile(dir, "deye", c.p)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want it to mention %q", name, err, c.want)
		}
	}
	// The serial number is optional.
	for _, host := range []string{"192.168.1.50", " 10.0.0.2 ", "fe80::1", "datalogger.local", "solarman-1"} {
		if err := saveProfile(dir, "deye", tcpProfile(host, 0)); err != nil {
			t.Errorf("%q without a serial number should save: %v", host, err)
		}
	}
	p, err := loadProfile("deye", dir)
	if err != nil || p.Modbus.TCPHost != "solarman-1" {
		t.Fatalf("saved host = %v (%v)", p, err)
	}
}

// Modbus TCP needs a valid address like Solarman; its port is optional
// (0 = 502), and it keeps it through a save.
func TestSaveProfileModbusTCP(t *testing.T) {
	dir := t.TempDir()
	p := tcpProfile("", 0)
	p.Modbus.Connection = profile.ConnModbusTCP
	if err := saveProfile(dir, "renac", p); err == nil || !strings.Contains(err.Error(), "IP address is empty") {
		t.Errorf("empty host: err = %v", err)
	}
	p.Modbus.TCPHost, p.Modbus.TCPPort = "192.168.1.60", 1502
	if err := saveProfile(dir, "renac", p); err != nil {
		t.Fatal(err)
	}
	got, err := loadProfile("renac", dir)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Modbus.IsNetwork() || got.Modbus.ModbusTCPPort() != 1502 {
		t.Errorf("saved modbus = %+v", got.Modbus)
	}
	got.Modbus.TCPPort = 0
	if got.Modbus.ModbusTCPPort() != 502 {
		t.Errorf("default port = %d, want 502", got.Modbus.ModbusTCPPort())
	}
}

// A copy of a built-in under an ID of its own keeps its base, and the
// inverter control drives it as that model.
func TestProfileCopyKeepsItsBase(t *testing.T) {
	dir := t.TempDir()
	p := tcpProfile("192.168.1.50", 0)
	p.Base = "bad id"
	if err := saveProfile(dir, "my_deye", p); err == nil || !strings.Contains(err.Error(), "base") {
		t.Errorf("bad base: err = %v", err)
	}
	p.Base = " deye_sg04lp3 "
	if err := saveProfile(dir, "my_deye", p); err != nil {
		t.Fatal(err)
	}
	got, err := loadProfile("my_deye", dir)
	if err != nil {
		t.Fatal(err)
	}
	if id := controlProfileID("my_deye", got); id != "deye_sg04lp3" {
		t.Errorf("copy driven as %q, want deye_sg04lp3", id)
	}
	if id := controlProfileID("deye_sg04lp3", tcpProfile("192.168.1.50", 0)); id != "deye_sg04lp3" {
		t.Errorf("built-in driven as %q", id)
	}
	if id := controlProfileID("", nil); id != "" {
		t.Errorf("no profile: %q", id)
	}
}

// A Deye profile seeded before fault_state existed is never rewritten on
// disk; loadProfile takes fault_state from the embedded copy instead.
func TestLoadProfileTakesBuiltinFaultState(t *testing.T) {
	dir := t.TempDir()
	if err := ensureDefaultProfiles(dir); err != nil {
		t.Fatal(err)
	}
	old, err := loadProfile("deye_sg04lp3", dir)
	if err != nil {
		t.Fatal(err)
	}
	old.FaultState = nil
	old.Faults = nil
	if err := saveProfile(dir, "deye_sg04lp3", old); err != nil {
		t.Fatal(err)
	}
	p, err := loadProfile("deye_sg04lp3", dir)
	if err != nil {
		t.Fatal(err)
	}
	if p.FaultState == nil || p.FaultState.Reading != "run_state" {
		t.Fatalf("FaultState = %+v, want the built-in run_state one", p.FaultState)
	}
	if len(p.Faults) == 0 || p.Faults[0].Group != "Device alarm" {
		t.Fatalf("Faults = %+v, want the built-in fault registers", p.Faults)
	}
	s, _ := loadProfile("swatten", dir)
	if s.FaultState != nil {
		t.Errorf("swatten has no fault_state, got %+v", s.FaultState)
	}
	if s.StatusNames["612"] != "Battery Abnormality" || s.StatusNames["831"] != "BMS Communication Abnormality" {
		t.Errorf("swatten status names 612/831 = %q/%q", s.StatusNames["612"], s.StatusNames["831"])
	}
}

// Every built-in profile parses, each embedded file is listed in
// builtinProfileIDs, and derived readings only use readings defined before them.
func TestBuiltinProfilesAreValid(t *testing.T) {
	files, err := builtinProfilesFS.ReadDir("orb/data/inverters")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != len(builtinProfileIDs) {
		t.Errorf("%d embedded profiles, %d in builtinProfileIDs", len(files), len(builtinProfileIDs))
	}
	for _, b := range builtinProfiles() {
		p, err := profile.Load(b.data)
		if err != nil {
			t.Errorf("%s: %v", b.id, err)
			continue
		}
		known := map[string]bool{}
		for _, r := range p.Registers {
			if known[r.Name] {
				t.Errorf("%s: reading %s defined twice", b.id, r.Name)
			}
			known[r.Name] = true
		}
		for _, d := range p.Derived {
			if (len(d.SumOf) == 0) == (len(d.ProductOf) == 0) {
				t.Errorf("%s: derived %s needs either sum_of or product_of", b.id, d.Name)
			}
			for _, src := range append(append([]string{}, d.SumOf...), d.ProductOf...) {
				if !known[src] {
					t.Errorf("%s: derived %s uses unknown reading %s", b.id, d.Name, src)
				}
			}
			known[d.Name] = true
		}
		if fs := p.FaultState; fs != nil && !known[fs.Reading] {
			t.Errorf("%s: fault_state reads unknown reading %s", b.id, fs.Reading)
		}
	}
}
