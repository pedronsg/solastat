package modbus

import (
	"context"
	"encoding/binary"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	sdkclient "github.com/OrbitOS-org/orbit-os-sdk-go/v26/client"
)

// fakePort stands in for the runtime's UART port: every Write is answered
// by respond (as chunks pushed into the listen stream, like the runtime
// delivers them), and the test can inject stray bytes or end the stream.
type fakePort struct {
	mu      sync.Mutex
	out     chan []byte
	respond func(req []byte) [][]byte
	listens int
	ended   bool
}

func (p *fakePort) Write(req []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.ended {
		return 0, errors.New("writeuart: port closed")
	}
	for _, chunk := range p.respond(req) {
		p.out <- chunk
	}
	return len(req), nil
}

func (p *fakePort) ListenAsync(context.Context, int) (<-chan []byte, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.listens++
	return p.out, nil
}

func (p *fakePort) Close() error { return nil }

// inject pushes bytes into the listen stream outside any request.
func (p *fakePort) inject(b []byte) { p.out <- b }

// endStream closes the listen stream, as when the runtime restarts.
func (p *fakePort) endStream() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.ended = true
	close(p.out)
}

// modbusSlave answers FC03/FC04 reads with each register holding its own
// address + offset, and echoes FC06/FC16 writes.
func modbusSlave(offset uint16) func(req []byte) [][]byte {
	return func(req []byte) [][]byte {
		slave, fc := req[0], req[1]
		addr := binary.BigEndian.Uint16(req[2:4])
		var resp []byte
		switch fc {
		case 3, 4:
			count := binary.BigEndian.Uint16(req[4:6])
			resp = []byte{slave, fc, byte(2 * count)}
			for i := uint16(0); i < count; i++ {
				resp = binary.BigEndian.AppendUint16(resp, addr+i+offset)
			}
			crc := crc16(resp)
			resp = append(resp, byte(crc), byte(crc>>8))
		case 6:
			resp = append([]byte(nil), req...)
		case 16:
			resp = append([]byte(nil), req[:6]...) // slave, fc, address, count
			crc := crc16(resp)
			resp = append(resp, byte(crc), byte(crc>>8))
		}
		return [][]byte{resp}
	}
}

// newTestClient returns a started Client whose ports come from newPort; it
// records every port opened.
func newTestClient(t *testing.T, newPort func() *fakePort) (*Client, *[]*fakePort) {
	t.Helper()
	oldSettle, oldFrame, oldRetry, oldTimeout := uartSettleDelay, interFrameDelay, retryDelay, requestTimeout
	uartSettleDelay, interFrameDelay, retryDelay, requestTimeout = 0, 0, time.Millisecond, 100*time.Millisecond
	t.Cleanup(func() {
		uartSettleDelay, interFrameDelay, retryDelay, requestTimeout = oldSettle, oldFrame, oldRetry, oldTimeout
	})

	var opened []*fakePort
	c := &Client{
		port: "ttyTEST",
		openPort: func(sdkclient.UartConfig) (uartPort, error) {
			p := newPort()
			opened = append(opened, p)
			return p, nil
		},
	}
	if err := c.Start(context.Background(), sdkclient.UartConfig{Baudrate: 9600}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Stop)
	return c, &opened
}

func newFake(respond func([]byte) [][]byte) func() *fakePort {
	return func() *fakePort { return &fakePort{out: make(chan []byte, 64), respond: respond} }
}

func TestRTUKeepsOneListenStream(t *testing.T) {
	c, opened := newTestClient(t, newFake(modbusSlave(0)))
	for i := 0; i < 5; i++ {
		regs, err := c.ReadRegisters(1, 4, 100, 2)
		if err != nil {
			t.Fatalf("read %d: %v", i, err)
		}
		if regs[0] != 100 || regs[1] != 101 {
			t.Fatalf("read %d = %v, want [100 101]", i, regs)
		}
	}
	if len(*opened) != 1 || (*opened)[0].listens != 1 {
		t.Fatalf("opened %d port(s) with %d listen stream(s), want 1 and 1", len(*opened), (*opened)[0].listens)
	}
}

func TestRTUDiscardsStaleBytes(t *testing.T) {
	c, opened := newTestClient(t, newFake(modbusSlave(0)))
	// A complete, valid-looking late response to some earlier request.
	stale := modbusSlave(5000)([]byte{1, 4, 0, 100, 0, 2, 0, 0})[0]
	(*opened)[0].inject(stale)

	regs, err := c.ReadRegisters(1, 4, 100, 2)
	if err != nil {
		t.Fatal(err)
	}
	if regs[0] != 100 {
		t.Fatalf("got %v: a stale response was taken for this one", regs)
	}
}

func TestRTUResponseInSmallChunks(t *testing.T) {
	slave := modbusSlave(0)
	c, _ := newTestClient(t, newFake(func(req []byte) [][]byte {
		var chunks [][]byte
		for _, b := range slave(req)[0] {
			chunks = append(chunks, []byte{b})
		}
		return chunks
	}))
	if _, err := c.ReadRegisters(1, 3, 10, 4); err != nil {
		t.Fatal(err)
	}
}

func TestRTUReopensWhenStreamEnds(t *testing.T) {
	c, opened := newTestClient(t, newFake(modbusSlave(0)))
	if _, err := c.ReadRegisters(1, 4, 100, 1); err != nil {
		t.Fatal(err)
	}
	(*opened)[0].endStream()

	if _, err := c.ReadRegisters(1, 4, 100, 1); err != nil {
		t.Fatalf("read after the stream ended should succeed on a reopened port: %v", err)
	}
	if len(*opened) != 2 {
		t.Fatalf("opened %d ports, want 2 (the reopen)", len(*opened))
	}
}

func TestRTUTimesOutWithoutResponse(t *testing.T) {
	c, _ := newTestClient(t, newFake(func([]byte) [][]byte { return nil }))
	_, err := c.ReadRegisters(1, 4, 100, 1)
	if err == nil || !strings.Contains(err.Error(), "no response") {
		t.Fatalf("err = %v, want a no-response timeout", err)
	}
}

func TestRTUExceptionResponse(t *testing.T) {
	c, _ := newTestClient(t, newFake(func(req []byte) [][]byte {
		resp := []byte{req[0], req[1] | 0x80, 2}
		crc := crc16(resp)
		return [][]byte{append(resp, byte(crc), byte(crc>>8))}
	}))
	start := time.Now()
	_, err := c.ReadRegisters(1, 4, 100, 1)
	if err == nil || !strings.Contains(err.Error(), "exception 2") {
		t.Fatalf("err = %v, want modbus exception 2", err)
	}
	if time.Since(start) > 50*time.Millisecond {
		t.Fatal("an exception response should end the request at once, not wait for the timeout")
	}
}

// Registers a model doesn't have answer with an exception: the link works,
// so the watchdog leaves the port alone; only link failures reopen it.
func TestRTUWatchdogIgnoresExceptions(t *testing.T) {
	refuse := func(req []byte) [][]byte {
		resp := []byte{req[0], req[1] | 0x80, 2}
		crc := crc16(resp)
		return [][]byte{append(resp, byte(crc), byte(crc>>8))}
	}
	c, opened := newTestClient(t, newFake(refuse))
	for i := 0; i < 2*watchdogThreshold; i++ {
		var exc *ExceptionError
		if _, err := c.ReadRegisters(1, 3, 100, 1); !errors.As(err, &exc) || exc.Code != 2 {
			t.Fatalf("read %d: err = %v, want an ExceptionError with code 2", i, err)
		}
	}
	if len(*opened) != 1 {
		t.Errorf("%d exceptions in a row opened the port %d times, want once", 2*watchdogThreshold, len(*opened))
	}

	c, opened = newTestClient(t, newFake(func([]byte) [][]byte { return nil }))
	for i := 0; i < watchdogThreshold; i++ {
		_, _ = c.ReadRegisters(1, 3, 100, 1)
	}
	if len(*opened) != 2 {
		t.Errorf("%d timeouts in a row opened the port %d times, want a reopen", watchdogThreshold, len(*opened))
	}
}

func TestRTUWriteRegister(t *testing.T) {
	c, _ := newTestClient(t, newFake(modbusSlave(0)))
	if err := c.WriteRegister(1, 23208, 0xAA); err != nil {
		t.Fatal(err)
	}
}

// The Modbus spec's own FC16 example: slave 0x11 writing 0x000A, 0x0102
// from register 1.
func TestBuildWriteMultiRequest_SpecExample(t *testing.T) {
	req, err := buildWriteMultiRequest(0x11, 1, []uint16{0x000A, 0x0102})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := toHex(req), "11 10 00 01 00 02 04 00 0a 01 02 c6 f0"; got != want {
		t.Fatalf("request = %s, want %s", got, want)
	}
}

func TestRTUWriteRegisters(t *testing.T) {
	var sent []byte
	slave := modbusSlave(0)
	c, _ := newTestClient(t, newFake(func(req []byte) [][]byte {
		sent = append([]byte(nil), req...)
		return slave(req)
	}))
	if err := c.WriteRegisters(1, 47100, []uint16{0x0001, 0x86A0, 60}); err != nil {
		t.Fatal(err)
	}
	if got, want := toHex(sent[:13]), "01 10 b7 fc 00 03 06 00 01 86 a0 00 3c"; got != want {
		t.Fatalf("request = %s, want %s", got, want)
	}
}

// A count FC16 can't carry fails before anything reaches the bus.
func TestRTUWriteRegistersRejectsBadCount(t *testing.T) {
	writes := 0
	c, _ := newTestClient(t, newFake(func(req []byte) [][]byte { writes++; return nil }))
	for _, n := range []int{0, maxWriteRegisters + 1} {
		if err := c.WriteRegisters(1, 100, make([]uint16, n)); err == nil {
			t.Errorf("%d values: expected an error", n)
		}
	}
	if writes != 0 {
		t.Fatalf("%d request(s) reached the bus", writes)
	}
}

// A slave echoing a different register count didn't take the whole write.
func TestRTUWriteRegistersEchoMismatch(t *testing.T) {
	c, _ := newTestClient(t, newFake(func(req []byte) [][]byte {
		resp := append([]byte(nil), req[:6]...)
		resp[5]-- // one register fewer than requested
		crc := crc16(resp)
		return [][]byte{append(resp, byte(crc), byte(crc>>8))}
	}))
	err := c.WriteRegisters(1, 100, []uint16{1, 2})
	if err == nil || !strings.Contains(err.Error(), "echo mismatch") {
		t.Fatalf("err = %v, want an echo mismatch", err)
	}
}
