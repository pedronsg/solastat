// Package solar provides the Data type and Poller that periodically reads all
// registers from the inverter via Modbus and broadcasts the result.
package solar

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"sync"
	"time"

	"solastat/internal/modbus"
	"solastat/internal/profile"

	"github.com/OrbitOS-org/orbit-os-sdk-go/v26/logger"
)

const logTag = "solar"
const interRequestDelay = 50 * time.Millisecond

// Reading holds the decoded value for a single register.
type Reading struct {
	Label string  `json:"label"`
	Value float64 `json:"value"`
	Unit  string  `json:"unit"`
}

// Data is the snapshot produced after each full poll cycle.
type Data struct {
	Ts       time.Time          `json:"ts"`
	Readings map[string]Reading `json:"readings"`
	Error    string             `json:"error,omitempty"`
	// Fault is nil when this cycle can't tell: the profile has neither an
	// inverter_status reading nor a fault_state, or it wasn't read.
	Fault *Fault `json:"fault,omitempty"`
}

// Fault is the inverter's own fault status in one poll cycle.
type Fault struct {
	Active bool `json:"active"`
	// Code is inverter_status' error code when Kind is "code", or the
	// fault_state reading's value (e.g. Deye's run state) when "state";
	// 0 when Kind is "faults" (only the profile's fault registers).
	Code int    `json:"code"`
	Kind string `json:"kind"`
	// Faults lists every fault and alarm active in this cycle, whatever the
	// source: inverter_status' code, the fault_state, and each one the
	// profile's fault registers (profile.FaultGroup) report. Empty when the
	// inverter reports none.
	Faults []ActiveFault `json:"faults,omitempty"`
}

// ActiveFault is one fault or alarm the inverter reports active.
type ActiveFault struct {
	// ID is the same for a given fault of a given profile on every cycle:
	// "<first register>.<bit>" for a bitmap, "<first register>=<code>" for
	// a code, "<reading>=<value>" for inverter_status or the fault_state.
	ID    string `json:"id"`
	Group string `json:"group"`          // e.g. "Device fault"
	Name  string `json:"name,omitempty"` // empty when the profile doesn't name it
	// Code is what the vendor calls the fault ("F18", "A01", "2032", "612"),
	// shown before its name; empty for an unnamed bit.
	Code     string `json:"code,omitempty"`
	Severity string `json:"severity"` // profile.FaultSeverityFault or ...Alarm
	Source   string `json:"source"`   // FaultFromStatus, FaultFromState or FaultFromRegisters
}

// Where an ActiveFault comes from: inverter_status' code, the profile's
// fault_state, or its fault registers (the only ones with names).
const (
	FaultFromStatus    = "status"
	FaultFromState     = "state"
	FaultFromRegisters = "registers"
)

// faultOf reads the fault status from inverter_status (an error code, 0 =
// OK, named by statusNames when the profile has them) or, for profiles
// without one, from the profile's fault_state.
func faultOf(readings map[string]Reading, fs *profile.FaultState, statusNames map[string]string) *Fault {
	if r, ok := readings["inverter_status"]; ok {
		f := &Fault{Active: r.Value != 0, Code: int(r.Value), Kind: "code"}
		if f.Active {
			f.Faults = []ActiveFault{{ID: fmt.Sprintf("inverter_status=%d", f.Code), Group: "Inverter status",
				Name: statusNames[strconv.Itoa(f.Code)], Code: strconv.Itoa(f.Code), Severity: profile.FaultSeverityFault, Source: FaultFromStatus}}
		}
		return f
	}
	if fs == nil {
		return nil
	}
	r, ok := readings[fs.Reading]
	if !ok {
		return nil
	}
	f := &Fault{Code: int(r.Value), Kind: "state"}
	for _, v := range fs.Values {
		if f.Code == v {
			f.Active = true
			f.Faults = []ActiveFault{{ID: fmt.Sprintf("%s=%d", fs.Reading, v), Group: "Inverter state", Severity: profile.FaultSeverityFault, Source: FaultFromState}}
		}
	}
	return f
}

// groupFaults decodes one profile.FaultGroup from its registers' words (in
// g.Registers order) into the faults it reports active.
func groupFaults(g profile.FaultGroup, words []uint16) []ActiveFault {
	var out []ActiveFault
	add := func(id, key string) {
		sev := g.Severity
		if s, ok := g.Severities[key]; ok {
			sev = s
		}
		code := g.Codes[key]
		if code == "" && g.Kind != profile.FaultBits {
			code = key // a code group's value is its own code
		}
		out = append(out, ActiveFault{ID: id, Group: g.Group, Name: g.Names[key], Code: code, Severity: sev, Source: FaultFromRegisters})
	}
	switch g.Kind {
	case profile.FaultBits:
		// Every bit set is "not available", not every fault at once: what
		// Huawei (ignore_invalid) and GoodWe (-1) send for an unused field.
		if allOnes(words) {
			return nil
		}
		for i, w := range words {
			for b := 0; b < 16; b++ {
				if w&(1<<b) != 0 {
					n := i*16 + b
					add(fmt.Sprintf("%d.%d", g.Registers[0], n), strconv.Itoa(n))
				}
			}
		}
	case profile.FaultCode:
		var v uint32
		for i, w := range words {
			v |= uint32(w) << (16 * i)
		}
		if !codeOK(g, v) {
			add(fmt.Sprintf("%d=%d", g.Registers[0], v), strconv.FormatUint(uint64(v), 10))
		}
	case profile.FaultCodes:
		// The same fault has the same ID whichever slot it's in.
		seen := map[uint16]bool{}
		for _, w := range words {
			if codeOK(g, uint32(w)) || seen[w] {
				continue
			}
			seen[w] = true
			add(fmt.Sprintf("%d=%d", g.Registers[0], w), strconv.Itoa(int(w)))
		}
	}
	return out
}

// codeOK reports whether v is one of g's no-fault codes (default 0).
func codeOK(g profile.FaultGroup, v uint32) bool {
	if len(g.OK) == 0 {
		return v == 0
	}
	for _, o := range g.OK {
		if v == o {
			return true
		}
	}
	return false
}

func allOnes(words []uint16) bool {
	for _, w := range words {
		if w != 0xFFFF {
			return false
		}
	}
	return len(words) > 0
}

// maxFaultSpan is how far apart a FaultGroup's registers can be and still
// be read in one request (otherwise one request each).
const maxFaultSpan = 32

// readFaults reads the profile's fault groups. read is false when there is
// nothing to read (no groups, or every one refused); failed when a group
// couldn't be read for a link error, so this cycle can't tell. A group the
// inverter refuses (a Modbus exception: a register this model doesn't
// have) is skipped from then on, once logged.
func (p *Poller) readFaults() (faults []ActiveFault, read, failed bool) {
	slaveID := p.prof.Modbus.SlaveAddress
	for i, g := range p.prof.Faults {
		if p.faultsOff[i] {
			continue
		}
		lo, hi := g.Registers[0], g.Registers[0]
		for _, r := range g.Registers {
			lo, hi = min(lo, r), max(hi, r)
		}
		if g.ReadCount > 0 {
			lo, hi = min(lo, g.ReadFrom), max(hi, g.ReadFrom+g.ReadCount-1)
		}
		byAddr := make(map[uint16]uint16, len(g.Registers))
		var err error
		if hi-lo < maxFaultSpan {
			var regs []uint16
			regs, err = p.mb.ReadRegisters(slaveID, g.FunctionCode, lo, hi-lo+1)
			for j, w := range regs {
				byAddr[lo+uint16(j)] = w
			}
			time.Sleep(interRequestDelay)
		} else {
			for _, r := range g.Registers {
				var regs []uint16
				if regs, err = p.mb.ReadRegisters(slaveID, g.FunctionCode, r, 1); err != nil {
					break
				}
				byAddr[r] = regs[0]
				time.Sleep(interRequestDelay)
			}
		}
		var exc *modbus.ExceptionError
		if errors.As(err, &exc) {
			logger.Warnf(logTag, "fault group %q (register %d): %v — not read again", g.Group, g.Registers[0], err)
			p.faultsOff[i] = true
			continue
		}
		if err != nil {
			logger.Warnf(logTag, "read fault group %q (register %d): %v", g.Group, g.Registers[0], err)
			return nil, true, true
		}
		words := make([]uint16, len(g.Registers))
		for j, r := range g.Registers {
			words[j] = byAddr[r]
		}
		read = true
		faults = append(faults, groupFaults(g, words)...)
	}
	return faults, read, false
}

// Poller reads all profile registers on a fixed interval and notifies via onData.
type Poller struct {
	mb   modbus.Access
	prof *profile.Profile
	// faultsOff marks the prof.Faults groups the inverter refused (see
	// readFaults). Only the poll goroutine touches it.
	faultsOff []bool
	mu        sync.RWMutex
	latest    Data
	onData    func(Data)
}

func NewPoller(mb modbus.Access, p *profile.Profile, onData func(Data)) *Poller {
	return &Poller{
		mb:        mb,
		prof:      p,
		faultsOff: make([]bool, len(p.Faults)),
		onData:    onData,
		latest:    Data{Readings: map[string]Reading{}},
	}
}

// Run polls in a loop until ctx is cancelled. The first poll fires immediately.
func (p *Poller) Run(ctx context.Context) {
	interval := time.Duration(p.prof.Modbus.PollIntervalMs) * time.Millisecond
	next := time.Now()
	for {
		now := time.Now()
		if wait := next.Sub(now); wait > 0 {
			select {
			case <-ctx.Done():
				return
			case <-time.After(wait):
			}
		}

		start := time.Now()
		p.poll()
		next = start.Add(interval)

		select {
		case <-ctx.Done():
			return
		default:
		}
	}
}

// Latest returns the most recently completed data snapshot (safe for concurrent reads).
func (p *Poller) Latest() Data {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.latest
}

func (p *Poller) poll() {
	data := Data{
		Ts:       time.Now(),
		Readings: make(map[string]Reading, len(p.prof.Registers)),
	}

	slaveID := p.prof.Modbus.SlaveAddress
	var lastErr string

	for _, reg := range p.prof.Registers {
		count := reg.ValueType.RegisterCount()
		regs, err := p.mb.ReadRegisters(slaveID, reg.FunctionCode, reg.Address, count)
		if err != nil {
			logger.Warnf(logTag, "read %s (addr %d): %v", reg.Name, reg.Address, err)
			lastErr = err.Error()
			// Stop this cycle on first comm error to avoid hammering UART.
			break
		}

		val := decodeValue(regs, reg.ValueType)
		if reg.Multiplier != 0 {
			val *= reg.Multiplier
		}

		data.Readings[reg.Name] = Reading{
			Label: reg.Label,
			Value: val,
			Unit:  reg.Unit,
		}

		time.Sleep(interRequestDelay) // inter-request delay for RS485 bus
	}

	signReadings(data.Readings, p.prof.Registers)

	// Compute derived readings (e.g. pv_total_power = pv1_total_power + pv2_total_power).
	for _, d := range p.prof.Derived {
		if len(d.ProductOf) > 0 {
			product, ok := 1.0, true
			for _, src := range d.ProductOf {
				r, found := data.Readings[src]
				if !found {
					ok = false
					break
				}
				product *= r.Value
			}
			if ok {
				data.Readings[d.Name] = Reading{Label: d.Label, Value: product, Unit: d.Unit}
			}
			continue
		}
		var sum float64
		for _, src := range d.SumOf {
			if r, ok := data.Readings[src]; ok {
				sum += r.Value
			}
		}
		data.Readings[d.Name] = Reading{
			Label: d.Label,
			Value: sum,
			Unit:  d.Unit,
		}
	}

	data.Error = lastErr
	data.Fault = faultOf(data.Readings, p.prof.FaultState, p.prof.StatusNames)
	// The fault registers only when the readings came through: after a link
	// error they'd just time out too.
	if lastErr == "" && len(p.prof.Faults) > 0 {
		faults, read, failed := p.readFaults()
		switch {
		case failed:
			data.Fault = nil // can't tell this cycle
		case read:
			if data.Fault == nil {
				data.Fault = &Fault{Kind: "faults"}
			}
			data.Fault.Faults = append(data.Fault.Faults, faults...)
			if len(faults) > 0 {
				data.Fault.Active = true
			}
		}
	}

	p.mu.Lock()
	p.latest = data
	p.mu.Unlock()

	if p.onData != nil {
		p.onData(data)
	}
}

// decodeValue converts raw register words into a float64 according to the value type.
// U_DWORD_R (reversed): register[0] = low word, register[1] = high word — matches ESPHome U_DWORD_R.
func decodeValue(regs []uint16, vt profile.ValueType) float64 {
	switch vt {
	case profile.UWord:
		return float64(regs[0])
	case profile.SWord:
		return float64(int16(regs[0]))
	case profile.UDword:
		raw := binary.BigEndian.Uint32([]byte{
			byte(regs[0] >> 8), byte(regs[0]),
			byte(regs[1] >> 8), byte(regs[1]),
		})
		return float64(raw)
	case profile.UDwordR: // low word first
		lo := uint32(regs[0])
		hi := uint32(regs[1]) << 16
		return float64(hi | lo)
	case profile.SDword: // int32, high word first
		raw := binary.BigEndian.Uint32([]byte{
			byte(regs[0] >> 8), byte(regs[0]),
			byte(regs[1] >> 8), byte(regs[1]),
		})
		return float64(int32(raw))
	case profile.SDwordR: // int32, low word first
		return float64(int32(uint32(regs[1])<<16 | uint32(regs[0])))
	}
	return math.NaN()
}

// The FaultCatalog kinds: what a profile can tell about the inverter's faults.
const (
	CatalogNamed = "registers" // fault registers
	CatalogCodes = "status"    // an inverter_status error code
	CatalogState = "state"     // only in fault or not (fault_state)
	CatalogNone  = "none"
)

// FaultCatalog lists every fault p can report that has a code or a name,
// with the same IDs the poller gives them (ActiveFault.ID), so a setting
// that stores IDs (a plugin's) matches what each poll reports. Unnamed
// bits aren't listed: they can still be reported, by number.
func FaultCatalog(p *profile.Profile) (kind string, faults []ActiveFault) {
	hasStatus := false
	for _, r := range p.Registers {
		if r.Name == "inverter_status" {
			hasStatus = true
		}
	}
	for _, g := range p.Faults {
		keys := map[string]bool{}
		for k := range g.Names {
			keys[k] = true
		}
		for k := range g.Codes {
			keys[k] = true
		}
		sorted := make([]string, 0, len(keys))
		for k := range keys {
			sorted = append(sorted, k)
		}
		sort.Slice(sorted, func(i, j int) bool { return numLess(sorted[i], sorted[j]) })
		for _, k := range sorted {
			id := fmt.Sprintf("%d=%s", g.Registers[0], k)
			if g.Kind == profile.FaultBits {
				id = fmt.Sprintf("%d.%s", g.Registers[0], k)
			}
			sev := g.Severity
			if s, ok := g.Severities[k]; ok {
				sev = s
			}
			code := g.Codes[k]
			if code == "" && g.Kind != profile.FaultBits {
				code = k
			}
			faults = append(faults, ActiveFault{ID: id, Group: g.Group, Name: g.Names[k], Code: code, Severity: sev, Source: FaultFromRegisters})
		}
	}
	if hasStatus {
		codes := make([]string, 0, len(p.StatusNames))
		for k := range p.StatusNames {
			codes = append(codes, k)
		}
		sort.Slice(codes, func(i, j int) bool { return numLess(codes[i], codes[j]) })
		for _, k := range codes {
			faults = append(faults, ActiveFault{ID: "inverter_status=" + k, Group: "Inverter status", Name: p.StatusNames[k], Code: k,
				Severity: profile.FaultSeverityFault, Source: FaultFromStatus})
		}
	} else if fs := p.FaultState; fs != nil {
		for _, v := range fs.Values {
			faults = append(faults, ActiveFault{ID: fmt.Sprintf("%s=%d", fs.Reading, v), Group: "Inverter state",
				Severity: profile.FaultSeverityFault, Source: FaultFromState})
		}
	}
	switch {
	case len(p.Faults) > 0:
		return CatalogNamed, faults
	case hasStatus:
		return CatalogCodes, faults
	case p.FaultState != nil:
		return CatalogState, faults
	}
	return CatalogNone, nil
}

// numLess orders decimal keys by value, others after them as strings.
func numLess(a, b string) bool {
	x, errA := strconv.Atoi(a)
	y, errB := strconv.Atoi(b)
	switch {
	case errA == nil && errB == nil:
		return x < y
	case errA == nil:
		return true
	case errB == nil:
		return false
	}
	return a < b
}

// signReadings gives each reading with a SignOf the sign of the reading it
// names, once every register is read (so their order doesn't matter).
func signReadings(readings map[string]Reading, regs []profile.Register) {
	for _, reg := range regs {
		if reg.SignOf == "" {
			continue
		}
		r, ok := readings[reg.Name]
		s, sok := readings[reg.SignOf]
		if !ok || !sok || s.Value == 0 {
			continue
		}
		r.Value = math.Copysign(r.Value, s.Value)
		readings[reg.Name] = r
	}
}
