package modbus

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/OrbitOS-org/orbit-os-sdk-go/v26/logger"
)

// Solarman V5: a WiFi/LAN datalogger dongle (port 8899) some inverters use
// instead of a local RS485 wire. It encapsulates a normal Modbus RTU
// request/response (with CRC16) inside its own framing, keyed to one
// specific dongle by its serial number:
//
//	[0]      0xA5
//	[1:3)    payload length, little-endian
//	[3:5)    control code 0x4510, little-endian
//	[5:7)    sequence number, little-endian
//	[7:11)   datalogger serial number, little-endian
//	[11]     frame type = 0x02 (request) — the response repeats this
//	[12]     status (response only)
//	[12:14)  sensor type = 0x0000 (request only, no status byte)
//	[14:26)  total working time / power-on time / offset time (3×uint32, zero)
//	[26:.)   the Modbus RTU frame itself (with its own CRC16)
//	[-2]     checksum: sum of every byte from offset 1 up to (not including)
//	         the checksum itself, mod 256
//	[-1]     0x15
//
// Ported from swattenErrorFix's solarman.cpp (ESP8266 firmware, tested
// against real Swatten/Solarman hardware) — same frame layout, same
// request/response validation.
const (
	solarmanPort        = 8899
	solarmanDialTimeout = 3 * time.Second
	solarmanIOTimeout   = 2 * time.Second
	solarmanStart       = 0xA5
	solarmanEnd         = 0x15
	// Control code 0x4510, as two literal little-endian bytes (0x10, 0x45)
	// — matching the reference firmware exactly, and sidestepping Go's
	// compile-time overflow check on a byte(uint16Constant) conversion.
	solarmanControlCodeLo = 0x10
	solarmanControlCodeHi = 0x45
)

// solarmanRetryDelay is the pause before a transaction's single retry on a
// fresh connection (a var so tests can shorten it).
var solarmanRetryDelay = 250 * time.Millisecond

// An unknown serial (0) is asked of the logger itself (learnSerialLocked):
// by the loggers' own discovery, "WIFIKIT-214028-READ" to UDP 48899,
// answered "ip,mac,serial" (the LSW-3's), else by a Solarman request with
// serial 0, which some loggers (the LSW-5) answer with their own serial in
// the header. Vars so tests can move them.
var (
	solarmanDiscoveryPort = 48899
	solarmanDiscoveryWait = 1500 * time.Millisecond
	solarmanLearnEvery    = 30 * time.Second // a failed attempt is retried this often, not on every request
)

const solarmanDiscoveryRequest = "WIFIKIT-214028-READ"

// ErrSerialUnknown: the profile has no serial for its logger, and the
// logger wouldn't tell it.
var ErrSerialUnknown = errors.New("the Solarman logger's serial number is unknown and the logger didn't tell it: type it from the logger's label")

// SolarmanClient implements Access over a Solarman V5 datalogger, as an
// alternative to Client's direct RS485/UART connection. Satisfies the same
// method set, so the poller and any plugin needing Modbus access don't
// care which one is active for the current profile.
//
// It keeps one TCP connection open and reuses it for every transaction,
// instead of dialling per request: a poll cycle is a dozen requests every
// few seconds, and that many short connections is exactly what makes
// dataloggers (and Wi-Fi links) drop or refuse one now and then. Any error
// closes the connection, so a late or partial response can never be read
// as the answer to the next request, and the transaction is retried once
// on a fresh connection (the datalogger may have closed an idle one).
type SolarmanClient struct {
	host   string
	port   int
	serial uint32

	mu   sync.Mutex // serializes transactions and guards conn, same contract as Client
	seq  uint16
	conn net.Conn // nil until the first transaction, and after any error

	// With no serial, the last attempt to learn it and its error; learned
	// is told the serial once it's learned.
	learnAt  time.Time
	learnErr error
	learned  func(serial uint32)
}

// NewSolarmanClient returns a client for the datalogger at host (its LAN
// IP or hostname; port 8899 is fixed by the protocol) identified by
// serial (the dongle's own serial number: the dongle rejects any other).
// With serial 0 the client asks the logger for it before its first request
// (OnSerialLearned).
func NewSolarmanClient(host string, serial uint32) *SolarmanClient {
	return &SolarmanClient{host: host, port: solarmanPort, serial: serial}
}

// OnSerialLearned sets f, told the serial (in a goroutine of its own) once
// a client made with none has learned it from the logger.
func (c *SolarmanClient) OnSerialLearned(f func(serial uint32)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.learned = f
}

// Stop closes the open connection, if any.
func (c *SolarmanClient) Stop() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closeConn()
}

func (c *SolarmanClient) closeConn() {
	if c.conn != nil {
		_ = c.conn.Close()
		c.conn = nil
	}
}

func (c *SolarmanClient) ReadRegisters(slaveID, fc byte, address, count uint16) ([]uint16, error) {
	req := buildRequest(slaveID, fc, address, count)
	resp, err := c.transact(req)
	if err != nil {
		return nil, err
	}
	return parseResponse(resp, slaveID, fc, count)
}

func (c *SolarmanClient) WriteRegister(slaveID byte, address, value uint16) error {
	req := buildRequest(slaveID, 6, address, value)
	resp, err := c.transact(req)
	if err != nil {
		return err
	}
	return parseWriteResponse(resp, slaveID, address, value)
}

func (c *SolarmanClient) WriteRegisters(slaveID byte, address uint16, values []uint16) error {
	req, err := buildWriteMultiRequest(slaveID, address, values)
	if err != nil {
		return err
	}
	resp, err := c.transact(req)
	if err != nil {
		return err
	}
	return parseWriteMultiResponse(resp, slaveID, address, uint16(len(values)))
}

// transact sends one Modbus RTU frame wrapped in a Solarman V5 envelope
// and returns the unwrapped, CRC-verified Modbus RTU response frame. On
// failure it retries once, on a fresh connection, after solarmanRetryDelay
// (a retried write just writes the same value again, which is harmless).
func (c *SolarmanClient) transact(mbReq []byte) ([]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.serial == 0 {
		if err := c.learnSerialLocked(); err != nil {
			return nil, err
		}
	}

	mb, err := c.transactOnce(mbReq)
	if err == nil {
		return mb, nil
	}
	logger.Debugf(logTag, "solarman %s: %v — retrying on a new connection", c.host, err)
	time.Sleep(solarmanRetryDelay)
	return c.transactOnce(mbReq)
}

// transactOnce runs one request/response on the open connection, dialling
// one first if needed. Any error closes the connection.
func (c *SolarmanClient) transactOnce(mbReq []byte) (mb []byte, err error) {
	defer func() {
		if err != nil {
			c.closeConn()
		}
	}()

	if c.conn == nil {
		conn, err := net.DialTimeout("tcp", net.JoinHostPort(c.host, fmt.Sprint(c.port)), solarmanDialTimeout)
		if err != nil {
			return nil, fmt.Errorf("dial solarman %s: %w", c.host, err)
		}
		c.conn = conn
	}
	_ = c.conn.SetDeadline(time.Now().Add(solarmanIOTimeout))

	c.seq++
	frame := solarmanEncode(c.seq, c.serial, mbReq)
	logger.Debugf(logTag, "solarman %s: writing frame: %s", c.host, toHex(frame))
	if _, err := c.conn.Write(frame); err != nil {
		return nil, fmt.Errorf("write solarman frame: %w", err)
	}

	resp, err := solarmanReadFrame(c.conn)
	if err != nil {
		return nil, fmt.Errorf("read solarman frame: %w", err)
	}
	logger.Debugf(logTag, "solarman %s: got frame: %s", c.host, toHex(resp))

	return solarmanDecode(resp, c.serial)
}

// learnSerialLocked asks the logger for its serial, at most once every
// solarmanLearnEvery: until then a failed attempt's error stands.
func (c *SolarmanClient) learnSerialLocked() error {
	if !c.learnAt.IsZero() && time.Since(c.learnAt) < solarmanLearnEvery {
		return c.learnErr
	}
	c.learnAt = time.Now()
	serial, err := solarmanSerialOf(c.host, c.port)
	if err != nil {
		c.learnErr = fmt.Errorf("%w (%s: %v)", ErrSerialUnknown, c.host, err)
		logger.Warnf(logTag, "solarman %s: no serial number, and the logger didn't tell it: %v", c.host, err)
		return c.learnErr
	}
	c.serial, c.learnErr = serial, nil
	logger.Infof(logTag, "solarman %s: serial number %d, told by the logger", c.host, serial)
	if c.learned != nil {
		go c.learned(serial)
	}
	return nil
}

// solarmanSerialOf asks the logger at host for its serial: its discovery
// first, then a Solarman request with serial 0.
func solarmanSerialOf(host string, port int) (uint32, error) {
	serial, err := solarmanDiscoverySerial(host)
	if err == nil {
		return serial, nil
	}
	serial, err2 := solarmanAnsweredSerial(host, port)
	if err2 == nil {
		return serial, nil
	}
	return 0, fmt.Errorf("discovery: %v; request: %v", err, err2)
}

// solarmanDiscoverySerial is the serial in the logger's answer to its
// discovery, sent to it alone: "ip,mac,serial".
func solarmanDiscoverySerial(host string) (uint32, error) {
	conn, err := net.Dial("udp", net.JoinHostPort(host, strconv.Itoa(solarmanDiscoveryPort)))
	if err != nil {
		return 0, err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(solarmanDiscoveryWait))
	if _, err := conn.Write([]byte(solarmanDiscoveryRequest)); err != nil {
		return 0, err
	}
	buf := make([]byte, 256)
	n, err := conn.Read(buf)
	if err != nil {
		return 0, err
	}
	f := strings.Split(strings.TrimSpace(string(buf[:n])), ",")
	if len(f) < 3 {
		return 0, fmt.Errorf("unexpected answer %q", buf[:n])
	}
	serial, err := strconv.ParseUint(strings.TrimSpace(f[2]), 10, 32)
	if err != nil || serial == 0 {
		return 0, fmt.Errorf("no serial in the answer %q", buf[:n])
	}
	return uint32(serial), nil
}

// solarmanAnsweredSerial sends the logger a Solarman request with serial 0
// (a read of one register) and returns the serial in its answer's header.
func solarmanAnsweredSerial(host string, port int) (uint32, error) {
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(host, strconv.Itoa(port)), solarmanDialTimeout)
	if err != nil {
		return 0, err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(solarmanIOTimeout))
	if _, err := conn.Write(solarmanEncode(1, 0, buildRequest(1, 3, 0, 1))); err != nil {
		return 0, err
	}
	resp, err := solarmanReadFrame(conn)
	if err != nil {
		return 0, err
	}
	if len(resp) < 11 || resp[0] != solarmanStart {
		return 0, fmt.Errorf("not a Solarman answer")
	}
	serial := binary.LittleEndian.Uint32(resp[7:11])
	if serial == 0 {
		return 0, fmt.Errorf("answered without its serial")
	}
	return serial, nil
}

// solarmanEncode wraps mbReq (a complete Modbus RTU request, CRC16
// included) in a Solarman V5 request frame.
func solarmanEncode(seq uint16, serial uint32, mbReq []byte) []byte {
	payload := make([]byte, 0, 14+len(mbReq))
	payload = append(payload, 0x02) // frame type
	payload = append(payload, 0x00, 0x00) // sensor type
	payload = append(payload, make([]byte, 12)...) // working/power-on/offset time
	payload = append(payload, mbReq...)

	frame := make([]byte, 0, 11+len(payload)+2)
	frame = append(frame, solarmanStart)
	frame = append(frame, byte(len(payload)), byte(len(payload)>>8))
	frame = append(frame, solarmanControlCodeLo, solarmanControlCodeHi)
	frame = append(frame, byte(seq), byte(seq>>8))
	frame = append(frame, byte(serial), byte(serial>>8), byte(serial>>16), byte(serial>>24))
	frame = append(frame, payload...)

	var checksum byte
	for _, b := range frame[1:] {
		checksum += b
	}
	frame = append(frame, checksum, solarmanEnd)
	return frame
}

// solarmanDecode validates a Solarman V5 response frame (start/end
// markers, declared length, serial number, checksum) and returns the
// Modbus RTU frame it carries, itself still CRC16-verified before return.
func solarmanDecode(resp []byte, wantSerial uint32) ([]byte, error) {
	if len(resp) < 13 {
		return nil, fmt.Errorf("solarman response too short: %d bytes", len(resp))
	}
	if resp[0] != solarmanStart {
		return nil, fmt.Errorf("solarman response: bad start byte 0x%02x", resp[0])
	}
	payloadLen := int(resp[1]) | int(resp[2])<<8
	total := 11 + payloadLen + 2
	if len(resp) < total {
		return nil, fmt.Errorf("solarman response: got %d bytes, declared length wants %d", len(resp), total)
	}
	if resp[total-1] != solarmanEnd {
		return nil, fmt.Errorf("solarman response: bad end byte 0x%02x", resp[total-1])
	}
	gotSerial := uint32(resp[7]) | uint32(resp[8])<<8 | uint32(resp[9])<<16 | uint32(resp[10])<<24
	if gotSerial != wantSerial {
		return nil, fmt.Errorf("solarman response: serial mismatch: got %d, want %d", gotSerial, wantSerial)
	}
	var checksum byte
	for _, b := range resp[1 : total-2] {
		checksum += b
	}
	if resp[total-2] != checksum {
		return nil, fmt.Errorf("solarman response: checksum mismatch: got 0x%02x, want 0x%02x", resp[total-2], checksum)
	}

	// A response payload starts with frame type + status (2 bytes) instead
	// of a request's frame type alone, so the Modbus frame is still at the
	// same fixed offset (11 header bytes + 14 payload preamble bytes = 25)
	// regardless of what the status byte says.
	const mbOffset = 25
	if payloadLen < 14 {
		return nil, fmt.Errorf("solarman response: payload too short for a Modbus frame: %d bytes", payloadLen)
	}
	mbLen := payloadLen - 14
	if mbLen < 4 {
		return nil, fmt.Errorf("solarman response: modbus frame too short: %d bytes", mbLen)
	}
	mb := resp[mbOffset : mbOffset+mbLen]

	crc := crc16(mb[:mbLen-2])
	if mb[mbLen-2] != byte(crc) || mb[mbLen-1] != byte(crc>>8) {
		return nil, fmt.Errorf("solarman response: modbus crc mismatch")
	}
	return mb, nil
}

// solarmanReadFrame reads one Solarman V5 frame from conn: the first 3
// bytes give the declared payload length, which fixes the total frame
// size (11 header + payload + 2 trailer).
func solarmanReadFrame(conn net.Conn) ([]byte, error) {
	buf := make([]byte, 0, 256)
	chunk := make([]byte, 256)
	for {
		n, err := conn.Read(chunk)
		if n > 0 {
			buf = append(buf, chunk[:n]...)
		}
		if len(buf) >= 3 {
			payloadLen := int(buf[1]) | int(buf[2])<<8
			want := 11 + payloadLen + 2
			if len(buf) >= want {
				return buf[:want], nil
			}
		}
		if err != nil {
			return nil, fmt.Errorf("%w (got %d bytes)", err, len(buf))
		}
	}
}
