package modbus

import (
	"bytes"
	"errors"
	"net"
	"sync/atomic"
	"testing"
	"time"
)

// buildSolarmanResponse constructs a valid Solarman V5 response frame
// wrapping mbResp (a complete Modbus RTU response, CRC16 included) — the
// mirror image of solarmanEncode, used here only to test solarmanDecode
// against something built independently of it.
func buildSolarmanResponse(serial uint32, mbResp []byte) []byte {
	// 14-byte preamble: frame type + status (1 byte each, not a separate
	// 2-byte sensor type like the request side) + 12 zero bytes — matching
	// solarmanDecode's mbOffset=25 (11-byte header + 14).
	payload := make([]byte, 0, 14+len(mbResp))
	payload = append(payload, 0x02, 0x00) // frame type + status
	payload = append(payload, make([]byte, 12)...)
	payload = append(payload, mbResp...)

	frame := make([]byte, 0, 11+len(payload)+2)
	frame = append(frame, solarmanStart)
	frame = append(frame, byte(len(payload)), byte(len(payload)>>8))
	frame = append(frame, solarmanControlCodeLo, solarmanControlCodeHi)
	frame = append(frame, 0x01, 0x00) // sequence
	frame = append(frame, byte(serial), byte(serial>>8), byte(serial>>16), byte(serial>>24))
	frame = append(frame, payload...)

	var checksum byte
	for _, b := range frame[1:] {
		checksum += b
	}
	frame = append(frame, checksum, solarmanEnd)
	return frame
}

func TestSolarmanEncodeThenDecodeRoundTrips(t *testing.T) {
	mbReq := buildRequest(1, 3, 100, 2) // slave 1, holding, addr 100, 2 regs

	frame := solarmanEncode(7, 123456789, mbReq)
	if frame[0] != solarmanStart || frame[len(frame)-1] != solarmanEnd {
		t.Fatalf("encoded frame missing start/end markers: %x", frame)
	}

	// Echo mbReq back as if it were the response payload (this test only
	// cares about the envelope, not real Modbus semantics).
	resp := buildSolarmanResponse(123456789, mbReq)
	got, err := solarmanDecode(resp, 123456789)
	if err != nil {
		t.Fatalf("solarmanDecode: %v", err)
	}
	if !bytes.Equal(got, mbReq) {
		t.Fatalf("solarmanDecode returned %x, want %x", got, mbReq)
	}
}

func TestSolarmanDecodeRejectsWrongSerial(t *testing.T) {
	mbReq := buildRequest(1, 3, 100, 2)
	resp := buildSolarmanResponse(111, mbReq)
	if _, err := solarmanDecode(resp, 222); err == nil {
		t.Fatal("expected an error for a serial number mismatch")
	}
}

func TestSolarmanDecodeRejectsBadChecksum(t *testing.T) {
	mbReq := buildRequest(1, 3, 100, 2)
	resp := buildSolarmanResponse(123, mbReq)
	resp[len(resp)-2] ^= 0xFF // corrupt the checksum byte
	if _, err := solarmanDecode(resp, 123); err == nil {
		t.Fatal("expected an error for a corrupted checksum")
	}
}

func TestSolarmanDecodeRejectsShortFrame(t *testing.T) {
	if _, err := solarmanDecode([]byte{0xA5, 0x00}, 123); err == nil {
		t.Fatal("expected an error for a too-short frame")
	}
}

// fakeDatalogger is a minimal Solarman V5 server on 127.0.0.1: it answers
// every FC03 read with zeros, and counts the connections it accepted.
// closeAfter > 0 closes each connection after that many answers, like a
// datalogger dropping an idle client.
type fakeDatalogger struct {
	ln         net.Listener
	serial     uint32
	closeAfter int
	accepted   atomic.Int32
}

func newFakeDatalogger(t *testing.T, serial uint32, closeAfter int) *fakeDatalogger {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	d := &fakeDatalogger{ln: ln, serial: serial, closeAfter: closeAfter}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			d.accepted.Add(1)
			go d.serve(conn)
		}
	}()
	return d
}

func (d *fakeDatalogger) serve(conn net.Conn) {
	defer conn.Close()
	for answered := 0; d.closeAfter == 0 || answered < d.closeAfter; answered++ {
		req, err := solarmanReadFrame(conn)
		if err != nil {
			return
		}
		mbReq := req[26 : len(req)-2] // after the 11-byte header and 15-byte request preamble
		var mbResp []byte
		if mbReq[1] == 16 {
			mbResp = append(mbResp, mbReq[:6]...) // echo slave, fc, address, count
		} else {
			count := int(mbReq[4])<<8 | int(mbReq[5])
			mbResp = []byte{mbReq[0], mbReq[1], byte(2 * count)}
			mbResp = append(mbResp, make([]byte, 2*count)...)
		}
		crc := crc16(mbResp)
		mbResp = append(mbResp, byte(crc), byte(crc>>8))
		if _, err := conn.Write(buildSolarmanResponse(d.serial, mbResp)); err != nil {
			return
		}
	}
}

func (d *fakeDatalogger) client() *SolarmanClient {
	c := NewSolarmanClient("127.0.0.1", d.serial)
	c.port = d.ln.Addr().(*net.TCPAddr).Port
	return c
}

func TestSolarmanClientReusesOneConnection(t *testing.T) {
	d := newFakeDatalogger(t, 42, 0)
	c := d.client()
	defer c.Stop()
	for i := 0; i < 5; i++ {
		if _, err := c.ReadRegisters(1, 3, 100, 2); err != nil {
			t.Fatalf("read %d: %v", i, err)
		}
	}
	if n := d.accepted.Load(); n != 1 {
		t.Fatalf("datalogger accepted %d connections, want 1", n)
	}
}

func TestSolarmanClientRetriesWhenConnectionDropped(t *testing.T) {
	old := solarmanRetryDelay
	solarmanRetryDelay = time.Millisecond
	t.Cleanup(func() { solarmanRetryDelay = old })

	d := newFakeDatalogger(t, 42, 1) // drops the client after every answer
	c := d.client()
	defer c.Stop()
	for i := 0; i < 3; i++ {
		if _, err := c.ReadRegisters(1, 3, 100, 2); err != nil {
			t.Fatalf("read %d should succeed on a fresh connection: %v", i, err)
		}
	}
	if n := d.accepted.Load(); n != 3 {
		t.Fatalf("datalogger accepted %d connections, want 3", n)
	}
}

func TestSolarmanClientReportsRefusedAfterRetry(t *testing.T) {
	old := solarmanRetryDelay
	solarmanRetryDelay = time.Millisecond
	t.Cleanup(func() { solarmanRetryDelay = old })

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close() // nothing listens there any more: every dial is refused
	c := NewSolarmanClient("127.0.0.1", 42)
	c.port = port
	if _, err := c.ReadRegisters(1, 3, 100, 2); err == nil {
		t.Fatal("expected an error when the datalogger refuses connections")
	}
}

func TestSolarmanClientWriteRegisters(t *testing.T) {
	d := newFakeDatalogger(t, 42, 0)
	c := d.client()
	defer c.Stop()
	if err := c.WriteRegisters(1, 47100, []uint16{1, 2, 3}); err != nil {
		t.Fatal(err)
	}
}

// noDiscovery points the loggers' discovery at a port nobody's on, for the
// test, and returns quickly when nothing answers it.
func noDiscovery(t *testing.T) {
	t.Helper()
	port, wait := solarmanDiscoveryPort, solarmanDiscoveryWait
	solarmanDiscoveryPort, solarmanDiscoveryWait = 1, 200*time.Millisecond
	t.Cleanup(func() { solarmanDiscoveryPort, solarmanDiscoveryWait = port, wait })
}

// With no serial, the client asks the logger: one that answers a request
// with serial 0 (as an LSW-5 does) tells it in the header, and the client
// carries on with it, telling OnSerialLearned.
func TestSolarmanClientLearnsTheSerial(t *testing.T) {
	noDiscovery(t)
	d := newFakeDatalogger(t, 3514707798, 0)
	c := d.client()
	c.serial = 0
	learned := make(chan uint32, 1)
	c.OnSerialLearned(func(s uint32) { learned <- s })
	defer c.Stop()

	if _, err := c.ReadRegisters(1, 3, 0, 2); err != nil {
		t.Fatal(err)
	}
	select {
	case s := <-learned:
		if s != 3514707798 {
			t.Fatalf("learned %d", s)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("OnSerialLearned not told")
	}
}

// The serial comes from the loggers' discovery when the logger answers it
// (as an LSW-3 does).
func TestSolarmanClientLearnsTheSerialByDiscovery(t *testing.T) {
	udp, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer udp.Close()
	go func() {
		buf := make([]byte, 64)
		n, from, err := udp.ReadFrom(buf)
		if err == nil && string(buf[:n]) == solarmanDiscoveryRequest {
			_, _ = udp.WriteTo([]byte("127.0.0.1,34EA34AABBCC,2712345678"), from)
		}
	}()
	port := solarmanDiscoveryPort
	solarmanDiscoveryPort = udp.LocalAddr().(*net.UDPAddr).Port
	defer func() { solarmanDiscoveryPort = port }()

	if s, err := solarmanSerialOf("127.0.0.1", 1); err != nil || s != 2712345678 {
		t.Fatalf("%d %v", s, err)
	}
}

// A logger that tells nothing leaves the serial unknown, said as such, and
// it isn't asked again on every request.
func TestSolarmanClientSerialUnknown(t *testing.T) {
	noDiscovery(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	var accepted atomic.Int32
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			accepted.Add(1)
			conn.Close() // an LSW-3 ignoring serial 0
		}
	}()
	c := NewSolarmanClient("127.0.0.1", 0)
	c.port = ln.Addr().(*net.TCPAddr).Port
	for range 3 {
		if _, err := c.ReadRegisters(1, 3, 0, 1); !errors.Is(err, ErrSerialUnknown) {
			t.Fatalf("err %v", err)
		}
	}
	if n := accepted.Load(); n != 1 {
		t.Fatalf("logger asked %d times", n)
	}
}
