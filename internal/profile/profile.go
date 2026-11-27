// Package profile defines the inverter profile schema and loader.
// A profile describes the UART config, Modbus slave address, poll interval,
// and the full register map for a specific inverter model.
package profile

import (
	"encoding/json"
	"fmt"
	"os"

	sdkclient "github.com/OrbitOS-org/orbit-os-sdk-go/v26/client"
)

// ValueType mirrors ESPHome modbus_controller value types.
type ValueType string

const (
	UWord   ValueType = "U_WORD"    // uint16, 1 register
	SWord   ValueType = "S_WORD"    // int16,  1 register
	UDword  ValueType = "U_DWORD"   // uint32, 2 registers, high word first
	UDwordR ValueType = "U_DWORD_R" // uint32, 2 registers, low word first (reversed)
	SDword  ValueType = "S_DWORD"   // int32,  2 registers, high word first
	SDwordR ValueType = "S_DWORD_R" // int32,  2 registers, low word first (reversed)
)

// RegisterCount returns how many Modbus registers this value type occupies.
func (vt ValueType) RegisterCount() uint16 {
	if vt == UDword || vt == UDwordR || vt == SDword || vt == SDwordR {
		return 2
	}
	return 1
}

// Register describes a single Modbus register (or pair for DWORD types).
type Register struct {
	// Name is the machine-readable key used in the SSE payload and energy flow mapping.
	// Well-known names for the dashboard: pv_total_power, measured_power,
	// battery_power, total_consumption_power, battery_level, battery_health,
	// solar_energy_total, grid_import_total, grid_export_total,
	// battery_charged_total, battery_discharged_total.
	// Voltages and currents the history keeps: grid_voltage, grid_current
	// (single-phase, or L1), grid_voltage_l2/_l3, grid_current_l2/_l3,
	// grid_frequency (Hz),
	// pvN_total_power_p_v / pvN_total_power_p_i (string N, 1-4),
	// battery_voltage, battery_current (same sign as battery_power).
	Name         string    `json:"name"`
	Label        string    `json:"label"`
	Address      uint16    `json:"address"`
	FunctionCode byte      `json:"function_code"` // 3 = holding, 4 = input
	ValueType    ValueType `json:"value_type"`
	Unit         string    `json:"unit"`
	// Multiplier is applied after decoding (e.g. 0.1 to convert raw 10ths to kWh).
	Multiplier float64 `json:"multiplier"`
	// SignOf, if set, names a reading whose sign this one takes: for a
	// magnitude whose direction the inverter gives elsewhere (Solis' battery
	// current, which takes its battery power's). Left as read while that
	// reading is 0 or missing.
	SignOf string `json:"sign_of,omitempty"`
}

// UartSettings holds the serial port configuration.
type UartSettings struct {
	BaudRate int    `json:"baud_rate"`
	DataBits int    `json:"data_bits"`
	Parity   string `json:"parity"` // "none", "even", "odd"
	StopBits int    `json:"stop_bits"`
}

// ModbusSettings holds Modbus-level parameters.
type ModbusSettings struct {
	SlaveAddress   byte `json:"slave_address"`
	PollIntervalMs int  `json:"poll_interval_ms"`
	// Connection selects the transport: "" or "rtu" (default — direct
	// RS485 over the device's own UART port), "tcp" (Solarman V5, for
	// inverters with a WiFi/LAN datalogger dongle instead of a local RS485
	// wire), "modbus_tcp" (plain Modbus TCP, for inverters with their own
	// Ethernet/Wi-Fi Modbus server) or "rtu_tcp" (Modbus RTU over TCP,
	// through a serial gateway that passes the frames through, e.g. a
	// Solarman dongle in Transparency mode). TCPHost is the address for the
	// network kinds; TCPSerial is the Solarman dongle's serial, TCPPort the
	// Modbus TCP port (0 = 502) or the gateway's (0 = 8899). SlaveAddress
	// doubles as Modbus TCP's unit id.
	Connection string `json:"connection,omitempty"`
	TCPHost    string `json:"tcp_host,omitempty"`
	TCPSerial  uint32 `json:"tcp_serial,omitempty"`
	TCPPort    uint16 `json:"tcp_port,omitempty"`
}

// The Connection values.
const (
	ConnRTU        = "rtu"
	ConnSolarman   = "tcp"
	ConnModbusTCP  = "modbus_tcp"
	ConnRTUOverTCP = "rtu_tcp"
)

// DefaultModbusTCPPort is Modbus TCP's standard port.
const DefaultModbusTCPPort = 502

// DefaultRTUOverTCPPort is the usual RTU-over-TCP gateway port — a Solarman
// dongle's, and Elfin/USR converters'.
const DefaultRTUOverTCPPort = 8899

// IsNetwork reports whether this profile connects over the network
// (Solarman V5, Modbus TCP or RTU over TCP) instead of the device's own
// UART.
func (m ModbusSettings) IsNetwork() bool {
	return m.Connection == ConnSolarman || m.Connection == ConnModbusTCP || m.Connection == ConnRTUOverTCP
}

// ModbusTCPPort is the Modbus TCP port to connect to: TCPPort, or 502.
func (m ModbusSettings) ModbusTCPPort() int {
	if m.TCPPort == 0 {
		return DefaultModbusTCPPort
	}
	return int(m.TCPPort)
}

// RTUOverTCPPort is the RTU-over-TCP gateway port to connect to: TCPPort,
// or 8899.
func (m ModbusSettings) RTUOverTCPPort() int {
	if m.TCPPort == 0 {
		return DefaultRTUOverTCPPort
	}
	return int(m.TCPPort)
}

// DerivedRegister defines a reading computed from other named readings after a poll cycle,
// in the order listed, so one can use an earlier one. Value = sum of the readings in SumOf
// (a missing one counts as 0), or with ProductOf, their product (e.g. a PV string's
// voltage × current), left out when any of them is missing.
type DerivedRegister struct {
	Name      string   `json:"name"`
	Label     string   `json:"label"`
	Unit      string   `json:"unit"`
	SumOf     []string `json:"sum_of,omitempty"`
	ProductOf []string `json:"product_of,omitempty"`
}

// FaultState tells when the inverter is in fault from a state reading, for
// models without an inverter_status register (0 = OK, anything else is that
// error code). The values are per model: Deye's run state 3 is "alarm" and
// 4 "fault", while Sofar's 3 is "EPS" and 4 "recoverable fault".
type FaultState struct {
	Reading string `json:"reading"` // name of one of the profile's readings
	Values  []int  `json:"values"`  // values of that reading that mean fault
}

// FaultGroup is a block of the inverter's fault or alarm registers, read on
// every poll so the faults it reports can be named: a bitmap (FaultBits, each
// set bit one active fault, as most models report them), a code (FaultCode,
// the value is the one active fault, e.g. INVT's or Solis') or code slots
// (FaultCodes, each register holds one active fault's code, e.g. SRNE's four).
//
// Registers are combined the way ha-solarman's definitions list them, so
// those convert as they are: the first is the lowest 16 bits, the next the
// 16 above, and so on. Bit N of a bitmap is bit N of that combined value.
type FaultGroup struct {
	Group        string   `json:"group"` // e.g. "Device fault", shown with each fault
	Registers    []uint16 `json:"registers"`
	FunctionCode byte     `json:"function_code"` // 3 = holding, 4 = input
	Kind         string   `json:"kind"`          // FaultBits, FaultCode or FaultCodes
	// Severity is FaultSeverityFault or FaultSeverityAlarm (a warning the
	// inverter keeps running through, e.g. a fan failure).
	Severity string `json:"severity"`
	// Names maps a bit index (FaultBits) or a code (FaultCode, FaultCodes),
	// in decimal, to the fault's name. A fault without one is still reported, by number.
	Names map[string]string `json:"names,omitempty"`
	// Codes maps the same keys to the code the vendor shows for the fault
	// (Deye's "F18", Afore's "A01", Huawei's "2032"), shown before its name.
	// A code or code slot without one shows its own value.
	Codes map[string]string `json:"codes,omitempty"`
	// Severities overrides Severity for single faults, keyed like Names:
	// Huawei's alarm registers mix major faults and warnings.
	Severities map[string]string `json:"severities,omitempty"`
	// OK lists the codes that mean no fault (FaultCode, FaultCodes);
	// default [0].
	// Solis' inverter status, for one, uses 0-3 for its normal states.
	OK []uint32 `json:"ok,omitempty"`
	// ReadFrom and ReadCount widen the request to registers the group
	// doesn't decode, for a model that only answers a block read whole:
	// Megarevo's 0x3100-0x3104. Zero when the registers alone are read.
	ReadFrom  uint16 `json:"read_from,omitempty"`
	ReadCount uint16 `json:"read_count,omitempty"`
}

// The FaultGroup kinds and severities.
const (
	FaultBits  = "bits"
	FaultCode  = "code"
	FaultCodes = "codes"

	FaultSeverityFault = "fault"
	FaultSeverityAlarm = "alarm"
)

// maxFaultRegisters bounds a FaultGroup: Sofar's 12-register fault bitmap
// is the largest one known.
const maxFaultRegisters = 16

func (g FaultGroup) validate() error {
	if g.Group == "" {
		return fmt.Errorf("fault group without a name")
	}
	if n := len(g.Registers); n == 0 || n > maxFaultRegisters {
		return fmt.Errorf("fault group %q: %d registers, want 1-%d", g.Group, n, maxFaultRegisters)
	}
	if g.FunctionCode != 3 && g.FunctionCode != 4 {
		return fmt.Errorf("fault group %q: function code %d, want 3 or 4", g.Group, g.FunctionCode)
	}
	if g.Kind != FaultBits && g.Kind != FaultCode && g.Kind != FaultCodes {
		return fmt.Errorf("fault group %q: kind %q, want %q, %q or %q", g.Group, g.Kind, FaultBits, FaultCode, FaultCodes)
	}
	if g.ReadCount > maxFaultRegisters*2 {
		return fmt.Errorf("fault group %q: read_count %d, want at most %d", g.Group, g.ReadCount, maxFaultRegisters*2)
	}
	if g.Kind == FaultCode && len(g.Registers) > 2 {
		return fmt.Errorf("fault group %q: a code is at most 2 registers", g.Group)
	}
	if g.Severity != FaultSeverityFault && g.Severity != FaultSeverityAlarm {
		return fmt.Errorf("fault group %q: severity %q, want %q or %q", g.Group, g.Severity, FaultSeverityFault, FaultSeverityAlarm)
	}
	for k, sev := range g.Severities {
		if sev != FaultSeverityFault && sev != FaultSeverityAlarm {
			return fmt.Errorf("fault group %q: severity %q of %s, want %q or %q", g.Group, sev, k, FaultSeverityFault, FaultSeverityAlarm)
		}
	}
	return nil
}

// Profile is the full configuration for one inverter model.
type Profile struct {
	Name string `json:"name"`
	// Base is the built-in profile this one is a copy of, by ID (e.g.
	// "deye_sg04lp3"), for a copy saved under an ID of its own: the plugins'
	// inverter control drives it as that model. Empty for the built-ins and
	// for a model of its own.
	Base       string            `json:"base,omitempty"`
	Uart       UartSettings      `json:"uart"`
	Modbus     ModbusSettings    `json:"modbus"`
	Registers  []Register        `json:"registers"`
	Derived    []DerivedRegister `json:"derived,omitempty"`
	FaultState *FaultState       `json:"fault_state,omitempty"`
	Faults     []FaultGroup      `json:"faults,omitempty"`
	// StatusNames names the inverter_status codes (decimal code → name), for
	// models whose error code register has a documented table (Swatten).
	StatusNames map[string]string `json:"status_names,omitempty"`
}

// UartConfig converts the profile's UART settings to the SDK type for the given port.
func (p *Profile) UartConfig(port string) sdkclient.UartConfig {
	parity := sdkclient.UartParityNone
	switch p.Uart.Parity {
	case "even":
		parity = sdkclient.UartParityEven
	case "odd":
		parity = sdkclient.UartParityOdd
	}
	stopBits := sdkclient.UartStopBits1
	if p.Uart.StopBits == 2 {
		stopBits = sdkclient.UartStopBits2
	}
	return sdkclient.UartConfig{
		Port:        port,
		Baudrate:    p.Uart.BaudRate,
		DataBits:    p.Uart.DataBits,
		Parity:      parity,
		StopBits:    stopBits,
		FlowControl: sdkclient.UartFlowNone,
	}
}

// Load parses a Profile from raw JSON bytes.
func Load(data []byte) (*Profile, error) {
	var p Profile
	if err := json.Unmarshal(data, &p); err != nil {
		return nil, fmt.Errorf("parse profile: %w", err)
	}
	if p.Modbus.PollIntervalMs <= 0 {
		p.Modbus.PollIntervalMs = 5000
	}
	for _, g := range p.Faults {
		if err := g.validate(); err != nil {
			return nil, fmt.Errorf("parse profile: %w", err)
		}
	}
	return &p, nil
}

// LoadFile reads and parses a Profile from a JSON file on disk.
func LoadFile(path string) (*Profile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read profile file %s: %w", path, err)
	}
	return Load(data)
}
