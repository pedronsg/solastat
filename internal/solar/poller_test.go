package solar

import (
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"solastat/internal/modbus"

	"solastat/internal/profile"
)

func TestFaultOf(t *testing.T) {
	deye := &profile.FaultState{Reading: "run_state", Values: []int{3, 4}}
	r := func(kv map[string]float64) map[string]Reading {
		m := map[string]Reading{}
		for k, v := range kv {
			m[k] = Reading{Value: v}
		}
		return m
	}
	tests := []struct {
		name     string
		readings map[string]Reading
		fs       *profile.FaultState
		want     *Fault
	}{
		{"status ok", r(map[string]float64{"inverter_status": 0}), nil, &Fault{Code: 0, Kind: "code"}},
		{"status code", r(map[string]float64{"inverter_status": 612}), nil, &Fault{Active: true, Code: 612, Kind: "code",
			Faults: []ActiveFault{{ID: "inverter_status=612", Group: "Inverter status", Code: "612", Severity: "fault", Source: "status"}}}},
		// inverter_status wins over fault_state when a profile has both.
		{"status wins", r(map[string]float64{"inverter_status": 0, "run_state": 4}), deye, &Fault{Code: 0, Kind: "code"}},
		{"deye normal", r(map[string]float64{"run_state": 2}), deye, &Fault{Code: 2, Kind: "state"}},
		{"deye alarm", r(map[string]float64{"run_state": 3}), deye, &Fault{Active: true, Code: 3, Kind: "state",
			Faults: []ActiveFault{{ID: "run_state=3", Group: "Inverter state", Severity: "fault", Source: "state"}}}},
		{"deye fault", r(map[string]float64{"run_state": 4}), deye, &Fault{Active: true, Code: 4, Kind: "state",
			Faults: []ActiveFault{{ID: "run_state=4", Group: "Inverter state", Severity: "fault", Source: "state"}}}},
		{"state not read", r(nil), deye, nil},
		{"no source", r(map[string]float64{"run_state": 4}), nil, nil},
	}
	for _, tt := range tests {
		got := faultOf(tt.readings, tt.fs, nil)
		if !reflect.DeepEqual(got, tt.want) {
			t.Errorf("%s: faultOf = %+v, want %+v", tt.name, got, tt.want)
		}
	}
}

func TestDecodeValueSignedDwordReversed(t *testing.T) {
	// −2 as int32 is 0xFFFFFFFE; S_DWORD_R holds the low word first.
	if got := decodeValue([]uint16{0xFFFE, 0xFFFF}, profile.SDwordR); got != -2 {
		t.Errorf("S_DWORD_R = %v, want -2", got)
	}
	if got := decodeValue([]uint16{0x86A0, 0x0001}, profile.SDwordR); got != 100000 {
		t.Errorf("S_DWORD_R = %v, want 100000", got)
	}
}

// fakeRegs answers each read with the words stored at its address.
type fakeRegs map[uint16][]uint16

func (f fakeRegs) ReadRegisters(_, _ byte, address, _ uint16) ([]uint16, error) {
	return f[address], nil
}
func (fakeRegs) WriteRegister(byte, uint16, uint16) error    { return nil }
func (fakeRegs) WriteRegisters(byte, uint16, []uint16) error { return nil }
func (fakeRegs) Stop()                                       {}

func TestPollDerivedProduct(t *testing.T) {
	prof := &profile.Profile{
		Registers: []profile.Register{
			{Name: "pv1_v", Address: 1, ValueType: profile.UWord, Multiplier: 0.1},
			{Name: "pv1_i", Address: 2, ValueType: profile.UWord, Multiplier: 0.1},
		},
		Derived: []profile.DerivedRegister{
			{Name: "pv1_total_power", ProductOf: []string{"pv1_v", "pv1_i"}},
			{Name: "pv_total_power", SumOf: []string{"pv1_total_power"}},
			{Name: "missing", ProductOf: []string{"pv1_v", "nope"}},
		},
	}
	// 300.0 V × 5.0 A
	p := NewPoller(fakeRegs{1: {3000}, 2: {50}}, prof, nil)
	p.poll()
	d := p.Latest()
	if got := d.Readings["pv1_total_power"].Value; math.Abs(got-1500) > 1e-9 {
		t.Errorf("pv1_total_power = %v, want 1500", got)
	}
	if got := d.Readings["pv_total_power"].Value; math.Abs(got-1500) > 1e-9 {
		t.Errorf("pv_total_power = %v, want 1500 (derived from a derived reading)", got)
	}
	if _, ok := d.Readings["missing"]; ok {
		t.Error("a product with a missing reading should be left out")
	}
}

func TestFaultOfNamesStatusCodes(t *testing.T) {
	names := map[string]string{"612": "Battery Abnormality"}
	f := faultOf(map[string]Reading{"inverter_status": {Value: 612}}, nil, names)
	if len(f.Faults) != 1 || f.Faults[0].Name != "Battery Abnormality" || f.Faults[0].ID != "inverter_status=612" {
		t.Errorf("faults = %+v, want inverter_status=612 named Battery Abnormality", f.Faults)
	}
	// A code the table doesn't list is still reported, unnamed.
	if f := faultOf(map[string]Reading{"inverter_status": {Value: 999}}, nil, names); f.Faults[0].Name != "" {
		t.Errorf("unlisted code named %q", f.Faults[0].Name)
	}
}

func TestGroupFaults(t *testing.T) {
	bits := profile.FaultGroup{Group: "Device fault", Registers: []uint16{103, 104}, Kind: profile.FaultBits,
		Severity: profile.FaultSeverityFault, Names: map[string]string{"1": "Fan failure", "17": "AC over-current"},
		Codes: map[string]string{"17": "F18"}, Severities: map[string]string{"1": profile.FaultSeverityAlarm}}
	// Bit 1 of the first register, bit 1 of the second (= bit 17), bit 3 unnamed.
	got := groupFaults(bits, []uint16{0b1010, 0b10})
	want := []ActiveFault{
		{ID: "103.1", Group: "Device fault", Name: "Fan failure", Severity: "alarm", Source: "registers"},
		{ID: "103.3", Group: "Device fault", Severity: "fault", Source: "registers"},
		{ID: "103.17", Group: "Device fault", Name: "AC over-current", Code: "F18", Severity: "fault", Source: "registers"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("bits = %+v, want %+v", got, want)
	}
	if got := groupFaults(bits, []uint16{0, 0}); got != nil {
		t.Errorf("no bit set = %+v, want none", got)
	}
	if got := groupFaults(bits, []uint16{0xFFFF, 0xFFFF}); got != nil {
		t.Errorf("all bits set (not available) = %+v, want none", got)
	}

	// A two-register code, low word first (INVT's 0x10001 = "FAN 1").
	code := profile.FaultGroup{Group: "Device alarm", Registers: []uint16{0x3808, 0x3807}, Kind: profile.FaultCode,
		Severity: profile.FaultSeverityAlarm, Names: map[string]string{"65537": "FAN 1"}}
	want = []ActiveFault{{ID: "14344=65537", Group: "Device alarm", Name: "FAN 1", Code: "65537", Severity: "alarm", Source: "registers"}}
	if got := groupFaults(code, []uint16{1, 1}); !reflect.DeepEqual(got, want) {
		t.Errorf("code = %+v, want %+v", got, want)
	}
	if got := groupFaults(code, []uint16{0, 0}); got != nil {
		t.Errorf("code 0 = %+v, want none", got)
	}
	// OK codes: Solis' normal states.
	solis := profile.FaultGroup{Group: "Inverter status", Registers: []uint16{3043}, Kind: profile.FaultCode,
		Severity: profile.FaultSeverityFault, OK: []uint32{0, 1, 2, 3}}
	if got := groupFaults(solis, []uint16{3}); got != nil {
		t.Errorf("Solis generating = %+v, want none", got)
	}
	if got := groupFaults(solis, []uint16{0x1010}); len(got) != 1 || got[0].ID != "3043=4112" {
		t.Errorf("Solis grid over-voltage = %+v, want 3043=4112", got)
	}

	// Code slots: SRNE's four, battery under-voltage and inverter overload
	// (the protocol's own example), the second one twice.
	srne := profile.FaultGroup{Group: "Fault code", Registers: []uint16{0x204, 0x205, 0x206, 0x207}, Kind: profile.FaultCodes,
		Severity: profile.FaultSeverityFault, Names: map[string]string{"1": "BatVoltLow", "14": "OverloadInverter"},
		Codes: map[string]string{"1": "01"}} // the manual's two digits; 14 shows its own value
	want = []ActiveFault{
		{ID: "516=1", Group: "Fault code", Name: "BatVoltLow", Code: "01", Severity: "fault", Source: "registers"},
		{ID: "516=14", Group: "Fault code", Name: "OverloadInverter", Code: "14", Severity: "fault", Source: "registers"},
	}
	if got := groupFaults(srne, []uint16{1, 14, 0, 14}); !reflect.DeepEqual(got, want) {
		t.Errorf("code slots = %+v, want %+v", got, want)
	}
	if got := groupFaults(srne, []uint16{0, 0, 0, 0}); got != nil {
		t.Errorf("empty slots = %+v, want none", got)
	}
}

// fakeWords answers a read of count registers from address with the words
// stored at each address (0 when absent), or errs[address].
type fakeWords struct {
	words map[uint16]uint16
	errs  map[uint16]error
	reads int
}

func (f *fakeWords) ReadRegisters(_, _ byte, address, count uint16) ([]uint16, error) {
	f.reads++
	if err := f.errs[address]; err != nil {
		return nil, err
	}
	out := make([]uint16, count)
	for i := range out {
		out[i] = f.words[address+uint16(i)]
	}
	return out, nil
}
func (*fakeWords) WriteRegister(byte, uint16, uint16) error    { return nil }
func (*fakeWords) WriteRegisters(byte, uint16, []uint16) error { return nil }
func (*fakeWords) Stop()                                       {}

func TestPollFaultGroups(t *testing.T) {
	prof := &profile.Profile{
		Registers: []profile.Register{{Name: "pv_total_power", Address: 1, ValueType: profile.UWord}},
		Faults: []profile.FaultGroup{
			// Out of order, like Afore's [0x1a, 0x1b, 0x1c, 0x19]: read in one request.
			{Group: "Alarm", Registers: []uint16{11, 10}, Kind: profile.FaultBits, Severity: profile.FaultSeverityAlarm},
			{Group: "Refused", Registers: []uint16{50}, Kind: profile.FaultBits, Severity: profile.FaultSeverityFault},
		},
	}
	mb := &fakeWords{
		words: map[uint16]uint16{1: 1000, 10: 0b1, 11: 0b100}, // bit 2 of 11 (first), bit 0 of 10 (second → 16)
		errs:  map[uint16]error{50: &modbus.ExceptionError{Code: 2, FC: 3}},
	}
	p := NewPoller(mb, prof, nil)
	p.poll()
	f := p.Latest().Fault
	if f == nil || !f.Active || f.Kind != "faults" {
		t.Fatalf("fault = %+v, want active, kind faults", f)
	}
	ids := []string{}
	for _, a := range f.Faults {
		ids = append(ids, a.ID)
	}
	if !reflect.DeepEqual(ids, []string{"11.2", "11.16"}) {
		t.Errorf("faults = %v, want [11.2 11.16]", ids)
	}

	// The refused group isn't read again; the readable one still is.
	before := mb.reads
	p.poll()
	if got := mb.reads - before; got != 2 {
		t.Errorf("second poll made %d reads, want 2 (the register and the alarm group)", got)
	}

	// A link error on a fault group: this cycle can't tell.
	mb.errs[10] = errors.New("timeout")
	p.poll()
	if f := p.Latest().Fault; f != nil {
		t.Errorf("fault after a link error = %+v, want nil", f)
	}

	// No fault set: known, not active.
	delete(mb.errs, 10)
	mb.words[10], mb.words[11] = 0, 0
	p.poll()
	if f := p.Latest().Fault; f == nil || f.Active || len(f.Faults) != 0 {
		t.Errorf("fault with none set = %+v, want known and inactive", f)
	}
}

// A group can widen its read: Megarevo answers 0x3100-0x3104 only read whole.
func TestFaultGroupReadWidening(t *testing.T) {
	prof := &profile.Profile{
		Registers: []profile.Register{{Name: "pv_total_power", Address: 1, ValueType: profile.UWord}},
		Faults: []profile.FaultGroup{{Group: "DSP fault", Registers: []uint16{0x3102, 0x3103}, Kind: profile.FaultBits,
			Severity: profile.FaultSeverityFault, ReadFrom: 0x3100, ReadCount: 5}},
	}
	mb := &recordingWords{fakeWords: fakeWords{words: map[uint16]uint16{0x3102: 0b100}}}
	p := NewPoller(mb, prof, nil)
	p.poll()
	if len(mb.calls) != 2 || mb.calls[1] != [2]uint16{0x3100, 5} {
		t.Errorf("reads = %v, want the register then 0x3100 x5", mb.calls)
	}
	if f := p.Latest().Fault; f == nil || len(f.Faults) != 1 || f.Faults[0].ID != "12546.2" {
		t.Errorf("fault = %+v, want bit 2 of 0x3102", f)
	}
}

type recordingWords struct {
	fakeWords
	calls [][2]uint16
}

func (r *recordingWords) ReadRegisters(slave, fc byte, address, count uint16) ([]uint16, error) {
	r.calls = append(r.calls, [2]uint16{address, count})
	return r.fakeWords.ReadRegisters(slave, fc, address, count)
}

// Every fault a built-in profile's catalog lists is reported with the same
// ID (and code, name, severity) when the inverter sets it: a plugin storing
// catalog IDs must match what each poll reports.
func TestFaultCatalogMatchesPolledFaults(t *testing.T) {
	files, err := filepath.Glob("../../cmd/solastat/orb/data/inverters/*.json")
	if err != nil || len(files) == 0 {
		t.Fatalf("no built-in profiles: %v", err)
	}
	for _, path := range files {
		p, err := profile.LoadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		kind, catalog := FaultCatalog(p)
		byID := map[string]ActiveFault{}
		for _, f := range catalog {
			if _, dup := byID[f.ID]; dup {
				t.Errorf("%s: %s listed twice", path, f.ID)
			}
			byID[f.ID] = f
		}
		for _, g := range p.Faults {
			for _, f := range catalog {
				if f.Group != g.Group || f.Source != FaultFromRegisters {
					continue
				}
				words := make([]uint16, len(g.Registers))
				var key uint64
				if g.Kind == profile.FaultBits {
					fmt.Sscanf(f.ID[strings.Index(f.ID, ".")+1:], "%d", &key)
					words[key/16] |= 1 << (key % 16)
				} else {
					fmt.Sscanf(f.ID[strings.Index(f.ID, "=")+1:], "%d", &key)
					for i := range words {
						words[i] = uint16(key >> (16 * i))
					}
				}
				got := groupFaults(g, words)
				if len(got) != 1 || !reflect.DeepEqual(got[0], f) {
					t.Errorf("%s: catalog %+v, polled %+v", path, f, got)
				}
			}
		}
		switch {
		case len(p.Faults) > 0 && kind != CatalogNamed,
			len(p.Faults) == 0 && len(p.StatusNames) > 0 && kind != CatalogCodes:
			t.Errorf("%s: kind %q", path, kind)
		}
	}
	// Swatten: its manual's codes, as faultOf reports them.
	p, _ := profile.LoadFile("../../cmd/solastat/orb/data/inverters/swatten.json")
	_, catalog := FaultCatalog(p)
	got := faultOf(map[string]Reading{"inverter_status": {Value: 612}}, nil, p.StatusNames).Faults[0]
	found := false
	for _, f := range catalog {
		if f.ID == got.ID {
			found = reflect.DeepEqual(f, got)
		}
	}
	if !found {
		t.Errorf("swatten 612: %+v not in the catalog as polled", got)
	}
}
