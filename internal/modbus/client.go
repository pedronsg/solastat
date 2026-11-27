// Package modbus implements a Modbus RTU client over an OrbitOS UART port.
// All reads are synchronous and serialised — call ReadRegisters from a single goroutine.
package modbus

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	sdkclient "github.com/OrbitOS-org/orbit-os-sdk-go/v26/client"
	"github.com/OrbitOS-org/orbit-os-sdk-go/v26/logger"
)

const logTag = "modbus"

// rxChunkSize bounds each chunk the runtime's listen stream delivers.
const rxChunkSize = 512

// Vars rather than consts so tests can shorten them.
var (
	// requestTimeout bounds how long a request waits for its response.
	requestTimeout = 2 * time.Second
	// uartSettleDelay is the pause after opening and after closing the port.
	uartSettleDelay = 500 * time.Millisecond
	// interFrameDelay is the quiet time kept before each request. Modbus
	// RTU needs only 3.5 characters (~4 ms at 9600 baud); the margin is for
	// inverters that are slow to turn their RS485 line around.
	interFrameDelay = 20 * time.Millisecond
	// retryDelay is the pause before a request's second attempt.
	retryDelay = 120 * time.Millisecond
)

// errStreamClosed means the port's listen stream ended (runtime restarted,
// port closed underneath us). Its text contains "listenuart" so the
// request's retry path reopens the port.
var errStreamClosed = errors.New("listenuart: rx stream closed")

// ExceptionError is the slave's own exception response: it answered, but
// refused the request (e.g. code 2, illegal data address, for a register
// the model doesn't have). The link itself is fine.
type ExceptionError struct {
	Code byte // the exception code
	FC   byte // the function code of the refused request
}

func (e *ExceptionError) Error() string {
	return fmt.Sprintf("modbus exception %d (fc 0x%02x)", e.Code, e.FC)
}

// uartPort is the part of *sdkclient.UartPort the client uses — an
// interface so tests can stand in a fake port.
type uartPort interface {
	Write(data []byte) (int, error)
	ListenAsync(ctx context.Context, maxChunkSize int) (<-chan []byte, error)
	Close() error
}

// Access is satisfied by Client (direct RS485 over the device's own UART),
// SolarmanClient (a WiFi/LAN datalogger dongle, see solarman.go) and
// TCPClient (the inverter's own Modbus TCP server, or an RTU-over-TCP
// gateway, see tcp.go) — the poller
// and any plugin needing register access don't care which transport a
// given profile is configured to use.
type Access interface {
	ReadRegisters(slaveID, fc byte, address, count uint16) ([]uint16, error)
	WriteRegister(slaveID byte, address, value uint16) error
	WriteRegisters(slaveID byte, address uint16, values []uint16) error
	Stop()
}

// Client wraps the SDK UartManager to provide Modbus RTU request/response semantics.
//
// The port's listen stream is opened once, right after the port itself, and
// stays open until Stop — an RX callback feeding rx — rather than a new
// stream per request. The runtime allows one listen stream per port and a
// cancelled one can take up to 200 ms to let go, so per-request streams
// could collide and hand a late response to the next request. Each request
// now drains whatever is left in rx, writes, and reads rx until the frame
// is complete.
type Client struct {
	openPort          func(cfg sdkclient.UartConfig) (uartPort, error)
	conn              uartPort      // nil unless a port is currently open
	rx                <-chan []byte // the open port's listen stream
	rxCancel          context.CancelFunc
	port              string
	mu                sync.Mutex
	stateMu           sync.Mutex
	baseCtx           context.Context
	cfg               sdkclient.UartConfig
	open              bool
	consecutiveErrors int
}

func NewClient(uart *sdkclient.UartManager, port string) *Client {
	return &Client{
		port: port,
		openPort: func(cfg sdkclient.UartConfig) (uartPort, error) {
			p, err := uart.Open(cfg)
			if err != nil {
				return nil, err
			}
			return p, nil
		},
	}
}

// Start opens the UART port and begins background byte accumulation.
func (c *Client) Start(ctx context.Context, cfg sdkclient.UartConfig) error {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	c.baseCtx = ctx
	c.cfg = cfg
	if err := c.openLocked("start"); err != nil {
		return err
	}
	// Extra delay after open to let inverter/port fully settle
	logger.Debugf(logTag, "Start: waiting %v for UART settle after open", uartSettleDelay)
	time.Sleep(uartSettleDelay)
	return nil
}

// Stop closes the UART stream and port. Any in-flight ReadRegisters call will
// return an error immediately once it notices the stopped flag.
func (c *Client) Stop() {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	c.closeLocked()
	time.Sleep(uartSettleDelay)
}

// closeLocked stops the listen stream, then closes the port. stateMu held.
func (c *Client) closeLocked() {
	if c.rxCancel != nil {
		c.rxCancel()
		c.rxCancel = nil
	}
	c.rx = nil
	if c.conn != nil {
		_ = c.conn.Close()
		c.conn = nil
	}
	c.open = false
}

// watchdogThreshold link failures in a row force a full port reopen.
const watchdogThreshold = 5

// watchdog counts a request's outcome (c.mu held): after watchdogThreshold
// failures in a row it forces a full port reopen, so a permanent UART loss
// (device offline, gRPC stream stuck, etc.) is recovered automatically
// without needing to restart the app. An exception response isn't one: the
// slave answered, so the link works — it only refused that register (one a
// model doesn't have), and reopening would just cut off whatever is next.
func (c *Client) watchdog(err error) {
	var exc *ExceptionError
	if err == nil || errors.As(err, &exc) {
		c.consecutiveErrors = 0
		return
	}
	c.consecutiveErrors++
	if c.consecutiveErrors >= watchdogThreshold {
		logger.Warnf(logTag, "uart %s: %d consecutive errors, watchdog forcing reopen", c.port, c.consecutiveErrors)
		c.consecutiveErrors = 0
		if rerr := c.reopen(); rerr != nil {
			logger.Warnf(logTag, "uart watchdog reopen failed on %s: %v", c.port, rerr)
		}
	}
}

// ReadRegisters sends a Modbus request (fc=3 holding or fc=4 input) and returns
// the decoded register values. Timeout is 2 s; add 50 ms between consecutive calls.
func (c *Client) ReadRegisters(slaveID, fc byte, address, count uint16) (regs []uint16, err error) {
	c.mu.Lock()
	defer func() {
		c.watchdog(err)
		c.mu.Unlock()
	}()

	req := buildRequest(slaveID, fc, address, count)
	for attempt := 1; attempt <= 2; attempt++ {
		logger.Debugf(logTag, "writing modbus request (attempt %d): slaveID=%d fc=%d addr=%d count=%d hex=%s",
			attempt, slaveID, fc, address, count, toHex(req))
		var raw []byte
		raw, err = c.requestOnce(req, count)
		if err == nil {
			logger.Debugf(logTag, "parsing response: %s", toHex(raw))
			regs, err = parseResponse(raw, slaveID, fc, count)
			return
		}
		if attempt == 1 && isTransientReadError(err) {
			logger.Debugf(logTag, "modbus attempt %d transient failure on %s: %v", attempt, c.port, err)
		} else {
			logger.Warnf(logTag, "modbus attempt %d failed on %s: %v", attempt, c.port, err)
		}
		if attempt == 1 {
			// Reopen on port-level or stream failures. Plain read timeouts are treated
			// as transient individually, but the watchdog above handles persistent timeouts.
			errMsg := strings.ToLower(err.Error())
			shouldReopen := strings.Contains(errMsg, "writeuart") ||
				strings.Contains(errMsg, "openuart") ||
				strings.Contains(errMsg, "listenuart") ||
				strings.Contains(errMsg, "input/output error")
			if shouldReopen {
				if rerr := c.reopen(); rerr != nil {
					logger.Warnf(logTag, "uart reopen failed on %s: %v", c.port, rerr)
				}
			} else {
				logger.Debugf(logTag, "skip uart reopen on transient read error: %v", err)
			}
			time.Sleep(retryDelay)
			continue
		}
		logger.Warnf(logTag, "modbus request failed after retries on %s: %v", c.port, err)
		return
	}
	err = fmt.Errorf("modbus request failed after retries")
	return
}

// WriteRegister sends a Modbus FC06 (write single holding register) request
// and waits for the slave's echo response.
func (c *Client) WriteRegister(slaveID byte, address, value uint16) error {
	req := buildRequest(slaveID, 6, address, value)
	return c.write(req, func(raw []byte) error { return parseWriteResponse(raw, slaveID, address, value) })
}

// WriteRegisters sends a Modbus FC16 (write multiple holding registers)
// request — for values that span registers (32-bit) or blocks an inverter
// only accepts written together — and waits for the slave's response, which
// echoes the start address and register count.
func (c *Client) WriteRegisters(slaveID byte, address uint16, values []uint16) error {
	req, err := buildWriteMultiRequest(slaveID, address, values)
	if err != nil {
		return err
	}
	return c.write(req, func(raw []byte) error {
		return parseWriteMultiResponse(raw, slaveID, address, uint16(len(values)))
	})
}

// write sends a write request and checks its response with parse. Same
// retry/reopen/watchdog structure as ReadRegisters, but the expected
// response is always 8 bytes (FC06 and FC16 both answer with an echo)
// rather than the read path's dynamic byte-count.
func (c *Client) write(req []byte, parse func(raw []byte) error) (err error) {
	c.mu.Lock()
	defer func() {
		c.watchdog(err)
		c.mu.Unlock()
	}()

	for attempt := 1; attempt <= 2; attempt++ {
		logger.Debugf(logTag, "writing modbus write request (attempt %d): hex=%s", attempt, toHex(req))
		var raw []byte
		raw, err = c.writeOnce(req)
		if err == nil {
			err = parse(raw)
			return
		}
		if attempt == 1 && isTransientReadError(err) {
			logger.Debugf(logTag, "modbus write attempt %d transient failure on %s: %v", attempt, c.port, err)
		} else {
			logger.Warnf(logTag, "modbus write attempt %d failed on %s: %v", attempt, c.port, err)
		}
		if attempt == 1 {
			errMsg := strings.ToLower(err.Error())
			shouldReopen := strings.Contains(errMsg, "writeuart") ||
				strings.Contains(errMsg, "openuart") ||
				strings.Contains(errMsg, "listenuart") ||
				strings.Contains(errMsg, "input/output error")
			if shouldReopen {
				if rerr := c.reopen(); rerr != nil {
					logger.Warnf(logTag, "uart reopen failed on %s: %v", c.port, rerr)
				}
			} else {
				logger.Debugf(logTag, "skip uart reopen on transient write error: %v", err)
			}
			time.Sleep(retryDelay)
			continue
		}
		logger.Warnf(logTag, "modbus write request failed after retries on %s: %v", c.port, err)
		return
	}
	err = fmt.Errorf("modbus write request failed after retries")
	return
}

// getConn returns the current *UartPort handle under stateMu, so it can
// never observe the half-written state of a concurrent Stop/reopen (e.g.
// the HTTP-triggered switchProfile calling Stop on this exact instance
// while the poller goroutine is mid-flight in ReadRegisters/WriteRegister
// — the package comment's "call from a single goroutine" is about
// ReadRegisters/WriteRegister themselves, not about Stop/reopen, which run
// from other goroutines by design). Without this, requestOnce/writeOnce
// read the c.conn field directly, unsynchronized with stateMu, and could
// dereference a nil *UartPort the instant Stop clears it — a panic in an
// HTTP-serving process takes the whole server down, not just the request.
func (c *Client) getConn() (uartPort, <-chan []byte, error) {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	if c.conn == nil || c.rx == nil {
		return nil, nil, fmt.Errorf("uart port not open")
	}
	return c.conn, c.rx, nil
}

// writeOnce performs one request/response cycle expecting a fixed 8-byte
// echo response (or a 5-byte exception response).
func (c *Client) writeOnce(req []byte) ([]byte, error) {
	return c.exchange(req, func(raw []byte) int {
		if len(raw) >= 2 && raw[1]&0x80 != 0 {
			return 5
		}
		return 8
	})
}

func parseWriteResponse(buf []byte, slaveID byte, address, value uint16) error {
	return parseWriteEcho(buf, slaveID, 6, address, value)
}

func parseWriteMultiResponse(buf []byte, slaveID byte, address, count uint16) error {
	return parseWriteEcho(buf, slaveID, 16, address, count)
}

// parseWriteEcho validates an 8-byte write response: slave, fc, then two
// big-endian words the slave echoes back — address and value for FC06,
// start address and register count for FC16.
func parseWriteEcho(buf []byte, slaveID, fc byte, address, value uint16) error {
	if len(buf) < 3 {
		return fmt.Errorf("short response: got %d bytes", len(buf))
	}
	if buf[0] != slaveID {
		return fmt.Errorf("slave id mismatch: got %d, want %d", buf[0], slaveID)
	}
	if buf[1]&0x80 != 0 {
		return &ExceptionError{Code: buf[2], FC: buf[1] & 0x7F}
	}
	if buf[1] != fc {
		return fmt.Errorf("fc mismatch: got 0x%02x, want 0x%02x", buf[1], fc)
	}
	const want = 8
	if len(buf) < want {
		return fmt.Errorf("short write response: got %d bytes, want %d", len(buf), want)
	}
	calc := crc16(buf[:want-2])
	got := uint16(buf[want-2]) | uint16(buf[want-1])<<8
	if calc != got {
		return fmt.Errorf("crc mismatch: calc=0x%04x got=0x%04x", calc, got)
	}
	gotAddr := binary.BigEndian.Uint16(buf[2:4])
	gotValue := binary.BigEndian.Uint16(buf[4:6])
	if gotAddr != address || gotValue != value {
		return fmt.Errorf("write echo mismatch: got addr=%d value=%d, want addr=%d value=%d", gotAddr, gotValue, address, value)
	}
	return nil
}

func (c *Client) openLocked(reason string) error {
	cfg := c.cfg
	cfg.Port = c.port
	logger.Debugf(logTag, "openLocked: configuring uart %s (%s): baud=%d, dataBits=%d, parity=%v, stopBits=%v, flowControl=%v",
		c.port, reason, cfg.Baudrate, cfg.DataBits, cfg.Parity, cfg.StopBits, cfg.FlowControl)

	// Best-effort cleanup in case this process still holds a stale handle
	// from a previous open. Ignore errors: port may already be closed.
	c.closeLocked()
	time.Sleep(200 * time.Millisecond)
	var openErr error
	for i := 1; i <= 3; i++ {
		conn, err := c.openPort(cfg)
		if err == nil {
			c.conn = conn
			openErr = nil
			break
		}
		openErr = err
		logger.Warnf(logTag, "open uart %s failed (%s, attempt %d/3): %v", c.port, reason, i, err)
		time.Sleep(time.Duration(i) * 120 * time.Millisecond)
	}
	if openErr != nil {
		return fmt.Errorf("open uart %s: %w", c.port, openErr)
	}

	// The listen stream lives as long as the port: derived from the app's
	// context (so shutdown ends it), cancelled by closeLocked.
	parent := c.baseCtx
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithCancel(parent)
	rx, err := c.conn.ListenAsync(ctx, rxChunkSize)
	if err != nil {
		cancel()
		_ = c.conn.Close()
		c.conn = nil
		return fmt.Errorf("listen uart %s: %w", c.port, err)
	}
	c.rx, c.rxCancel = rx, cancel
	c.open = true
	logger.Infof(logTag, "uart %s open ok (%s) - baud=%d, dataBits=%d, parity=%v, stopBits=%v",
		c.port, reason, cfg.Baudrate, cfg.DataBits, cfg.Parity, cfg.StopBits)
	return nil
}

func (c *Client) reopen() error {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	c.closeLocked()
	time.Sleep(uartSettleDelay)
	return c.openLocked("reopen")
}

func (c *Client) requestOnce(req []byte, count uint16) ([]byte, error) {
	return c.exchange(req, func(raw []byte) int {
		if len(raw) >= 3 {
			if raw[1]&0x80 != 0 {
				return 5 // exception response
			}
			return 5 + int(raw[2]) // address, fc, byte count, data, crc
		}
		return 5 + int(count)*2
	})
}

// exchange writes req and reads the port's listen stream until frameLen
// (recomputed as bytes arrive) says the response is complete, or
// requestTimeout passes. Anything left over from an earlier exchange is
// discarded first, so it can't be taken for this response.
func (c *Client) exchange(req []byte, frameLen func(raw []byte) int) ([]byte, error) {
	conn, rx, err := c.getConn()
	if err != nil {
		return nil, err
	}
	// Quiet time first, then drain: a late response landing during the pause
	// is discarded too, not taken for this one.
	time.Sleep(interFrameDelay)
	if err := drain(rx); err != nil {
		return nil, err
	}

	logger.Debugf(logTag, "exchange: writing request: %s", toHex(req))
	if _, err := conn.Write(req); err != nil {
		return nil, fmt.Errorf("write modbus request: %w", err)
	}

	timer := time.NewTimer(requestTimeout)
	defer timer.Stop()
	var raw []byte
	want := frameLen(nil)
	for {
		select {
		case data, ok := <-rx:
			if !ok {
				return nil, errStreamClosed
			}
			logger.Debugf(logTag, "exchange: received %d bytes: %s", len(data), toHex(data))
			raw = trimToSlaveFrame(append(raw, data...), req[0])
			want = frameLen(raw)
			if len(raw) >= want {
				logger.Debugf(logTag, "exchange: complete response: %s", toHex(raw[:want]))
				return raw[:want], nil
			}
		case <-timer.C:
			if len(raw) == 0 {
				return nil, fmt.Errorf("no response received (timeout)")
			}
			return nil, fmt.Errorf("partial modbus response: got %d bytes, want %d (raw=%s)", len(raw), want, toHex(raw))
		}
	}
}

// drain discards whatever the listen stream has buffered, without waiting.
func drain(rx <-chan []byte) error {
	for {
		select {
		case data, ok := <-rx:
			if !ok {
				return errStreamClosed
			}
			logger.Debugf(logTag, "drain: discarded %d stale bytes: %s", len(data), toHex(data))
		default:
			return nil
		}
	}
}

func buildRequest(slaveID, fc byte, address, count uint16) []byte {
	pdu := make([]byte, 6)
	pdu[0] = slaveID
	pdu[1] = fc
	binary.BigEndian.PutUint16(pdu[2:], address)
	binary.BigEndian.PutUint16(pdu[4:], count)
	crc := crc16(pdu)
	return append(pdu, byte(crc), byte(crc>>8)) // CRC is little-endian on the wire
}

// maxWriteRegisters is FC16's limit on registers per request.
const maxWriteRegisters = 123

// buildWriteMultiRequest builds an FC16 request: slave, fc, start address,
// register count, byte count, the values, CRC.
func buildWriteMultiRequest(slaveID byte, address uint16, values []uint16) ([]byte, error) {
	if len(values) == 0 || len(values) > maxWriteRegisters {
		return nil, fmt.Errorf("write multiple registers: %d values, want 1-%d", len(values), maxWriteRegisters)
	}
	req := []byte{slaveID, 16}
	req = binary.BigEndian.AppendUint16(req, address)
	req = binary.BigEndian.AppendUint16(req, uint16(len(values)))
	req = append(req, byte(2*len(values)))
	for _, v := range values {
		req = binary.BigEndian.AppendUint16(req, v)
	}
	crc := crc16(req)
	return append(req, byte(crc), byte(crc>>8)), nil
}

func parseResponse(buf []byte, slaveID, fc byte, count uint16) ([]uint16, error) {
	want := 5 + int(count)*2
	logger.Debugf(logTag, "parseResponse: slaveID=0x%02x fc=0x%02x count=%d, want=%d bytes, got=%d bytes", slaveID, fc, count, want, len(buf))

	// An exception response is only 5 bytes, so check for one before the
	// length a normal response needs.
	if len(buf) >= 5 && buf[0] == slaveID && buf[1]&0x80 != 0 {
		exceptionCode := buf[2]
		logger.Errorf(logTag, "parseResponse: EXCEPTION response received: code=%d (0x%02x)", exceptionCode, exceptionCode)
		return nil, &ExceptionError{Code: buf[2], FC: buf[1] & 0x7F}
	}

	if len(buf) < want {
		return nil, fmt.Errorf("short response: got %d bytes, want %d", len(buf), want)
	}

	logger.Debugf(logTag, "parseResponse: checking slave id: buf[0]=0x%02x vs expected=0x%02x", buf[0], slaveID)
	if buf[0] != slaveID {
		return nil, fmt.Errorf("slave id mismatch: got %d, want %d", buf[0], slaveID)
	}

	logger.Debugf(logTag, "parseResponse: checking fc: buf[1]=0x%02x vs expected=0x%02x", buf[1], fc)

	if buf[1] != fc {
		return nil, fmt.Errorf("fc mismatch: got 0x%02x, want 0x%02x", buf[1], fc)
	}

	logger.Debugf(logTag, "parseResponse: checking CRC")
	calc := crc16(buf[:want-2])
	got := uint16(buf[want-2]) | uint16(buf[want-1])<<8
	logger.Debugf(logTag, "parseResponse: CRC - calculated=0x%04x received=0x%04x", calc, got)
	if calc != got {
		return nil, fmt.Errorf("crc mismatch: calc=0x%04x got=0x%04x", calc, got)
	}

	regs := make([]uint16, count)
	for i := range regs {
		regs[i] = binary.BigEndian.Uint16(buf[3+2*i:])
		logger.Debugf(logTag, "parseResponse: reg[%d] = 0x%04x (%d)", i, regs[i], regs[i])
	}
	logger.Debugf(logTag, "parseResponse: success, parsed %d registers", count)
	return regs, nil
}

func toHex(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	enc := make([]byte, hex.EncodedLen(len(b)))
	hex.Encode(enc, b)
	out := make([]byte, 0, len(enc)+len(enc)/2)
	for i := 0; i < len(enc); i += 2 {
		if i > 0 {
			out = append(out, ' ')
		}
		out = append(out, enc[i], enc[i+1])
	}
	return string(out)
}

func trimToSlaveFrame(raw []byte, slaveID byte) []byte {
	if len(raw) == 0 {
		return raw
	}
	// Skip leading noise bytes until the expected slave id.
	for i := 0; i < len(raw); i++ {
		if raw[i] == slaveID {
			if i > 0 {
				logger.Debugf(logTag, "trimToSlaveFrame: discarded %d noise bytes before slave ID 0x%02x: %s", i, slaveID, toHex(raw[:i]))
			}
			return raw[i:]
		}
	}
	// No slave-id found; treat as noise.
	logger.Warnf(logTag, "trimToSlaveFrame: no slave ID 0x%02x found in buffer, discarding all %d bytes: %s", slaveID, len(raw), toHex(raw))
	return nil
}

func isTransientReadError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "no response received (timeout)") ||
		strings.Contains(msg, "partial modbus response")
}
