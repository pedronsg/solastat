package modbus

import (
	"encoding/binary"
	"io"
	"net"
	"strconv"
	"sync"
	"testing"
	"time"
)

// fakeModbusTCP is a minimal Modbus TCP server — or, with rtu, an
// RTU-over-TCP gateway with its slave behind it: register N reads as
// N+1000, writes are stored, address 9999 answers with exception 2, and
// dropAfter > 0 closes each connection after that many requests. badCRC
// spoils the CRC of RTU responses.
type fakeModbusTCP struct {
	ln        net.Listener
	rtu       bool
	badCRC    bool
	mu        sync.Mutex
	written   map[uint16]uint16
	conns     int
	dropAfter int
}

func startFakeModbusTCP(t *testing.T) *fakeModbusTCP {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &fakeModbusTCP{ln: ln, written: map[uint16]uint16{}}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			s.mu.Lock()
			s.conns++
			s.mu.Unlock()
			go s.serve(conn)
		}
	}()
	return s
}

func (s *fakeModbusTCP) client(t *testing.T) *TCPClient {
	t.Helper()
	host, port, _ := net.SplitHostPort(s.ln.Addr().String())
	p, _ := strconv.Atoi(port)
	c := NewTCPClient(host, p)
	if s.rtu {
		c = NewRTUOverTCPClient(host, p)
	}
	t.Cleanup(c.Stop)
	return c
}

// readRequest reads one request and returns its PDU, and what goes before
// the response's PDU: the MBAP header (length still to fill in), or the
// RTU slave address.
func (s *fakeModbusTCP) readRequest(r io.Reader) (prefix, pdu []byte, err error) {
	if !s.rtu {
		header := make([]byte, mbapLen)
		if _, err := io.ReadFull(r, header); err != nil {
			return nil, nil, err
		}
		pdu := make([]byte, int(binary.BigEndian.Uint16(header[4:]))-1)
		if _, err := io.ReadFull(r, pdu); err != nil {
			return nil, nil, err
		}
		return header, pdu, nil
	}
	frame := make([]byte, 7, 260) // slave, fc, address, count (or value), FC16's byte count
	if _, err := io.ReadFull(r, frame[:6]); err != nil {
		return nil, nil, err
	}
	n, have := 8, 6
	if frame[1] == 16 {
		if _, err := io.ReadFull(r, frame[6:7]); err != nil {
			return nil, nil, err
		}
		n, have = 9+int(frame[6]), 7
	}
	frame = frame[:n]
	if _, err := io.ReadFull(r, frame[have:]); err != nil {
		return nil, nil, err
	}
	if crc := crc16(frame[:n-2]); frame[n-2] != byte(crc) || frame[n-1] != byte(crc>>8) {
		return nil, nil, io.ErrUnexpectedEOF
	}
	return frame[:1], frame[1 : n-2], nil
}

// response frames a response PDU after prefix, as readRequest returned it.
func (s *fakeModbusTCP) response(prefix, out []byte) []byte {
	if !s.rtu {
		resp := make([]byte, 6, 7+len(out))
		copy(resp, prefix[:4])
		binary.BigEndian.PutUint16(resp[4:], uint16(1+len(out)))
		resp = append(resp, prefix[6])
		return append(resp, out...)
	}
	resp := append([]byte{prefix[0]}, out...)
	crc := crc16(resp)
	if s.badCRC {
		crc++
	}
	return append(resp, byte(crc), byte(crc>>8))
}

func (s *fakeModbusTCP) serve(conn net.Conn) {
	defer conn.Close()
	for n := 1; ; n++ {
		prefix, pdu, err := s.readRequest(conn)
		if err != nil {
			return
		}
		fc, addr := pdu[0], binary.BigEndian.Uint16(pdu[1:])
		var out []byte
		switch {
		case addr == 9999:
			out = []byte{fc | 0x80, 2}
		case fc == 3 || fc == 4:
			count := binary.BigEndian.Uint16(pdu[3:])
			out = []byte{fc, byte(2 * count)}
			for i := uint16(0); i < count; i++ {
				out = binary.BigEndian.AppendUint16(out, addr+i+1000)
			}
		case fc == 6:
			s.mu.Lock()
			s.written[addr] = binary.BigEndian.Uint16(pdu[3:])
			s.mu.Unlock()
			out = pdu // echo
		case fc == 16:
			count := binary.BigEndian.Uint16(pdu[3:])
			s.mu.Lock()
			for i := uint16(0); i < count; i++ {
				s.written[addr+i] = binary.BigEndian.Uint16(pdu[6+2*i:])
			}
			s.mu.Unlock()
			out = pdu[:5]
		}
		if _, err := conn.Write(s.response(prefix, out)); err != nil {
			return
		}
		if s.dropAfter > 0 && n >= s.dropAfter {
			return
		}
	}
}

func TestTCPClientReadsAndWrites(t *testing.T) {
	s := startFakeModbusTCP(t)
	c := s.client(t)

	regs, err := c.ReadRegisters(1, 4, 0x16, 2)
	if err != nil {
		t.Fatal(err)
	}
	if regs[0] != 0x16+1000 || regs[1] != 0x17+1000 {
		t.Errorf("regs = %v", regs)
	}
	if err := c.WriteRegister(1, 0xB9, 5); err != nil {
		t.Fatal(err)
	}
	if err := c.WriteRegisters(1, 0x100, []uint16{5, 4, 0xF448}); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.written[0xB9] != 5 || s.written[0x100] != 5 || s.written[0x101] != 4 || s.written[0x102] != 0xF448 {
		t.Errorf("written = %v", s.written)
	}
	if s.conns != 1 {
		t.Errorf("%d connections, want the one kept open", s.conns)
	}
}

func TestTCPClientReportsExceptions(t *testing.T) {
	s := startFakeModbusTCP(t)
	if _, err := s.client(t).ReadRegisters(1, 3, 9999, 1); err == nil {
		t.Fatal("an exception response must be an error")
	}
}

// The server closing the connection (idle timeout, restart) costs one
// retry on a fresh connection, not a failed read.
func TestTCPClientReconnectsAfterTheServerCloses(t *testing.T) {
	s := startFakeModbusTCP(t)
	s.dropAfter = 1
	tcpRetryDelay = time.Millisecond
	c := s.client(t)
	for i := 0; i < 3; i++ {
		if _, err := c.ReadRegisters(1, 4, uint16(i), 1); err != nil {
			t.Fatalf("read %d: %v", i, err)
		}
	}
}

func TestMBAPEncodeDropsTheCRC(t *testing.T) {
	frame := mbapEncode(7, buildRequest(1, 4, 0x50, 1))
	want := []byte{0, 7, 0, 0, 0, 6, 1, 4, 0, 0x50, 0, 1}
	if string(frame) != string(want) {
		t.Errorf("frame = % x, want % x", frame, want)
	}
}

func TestRTUOverTCPClientReadsAndWrites(t *testing.T) {
	s := startFakeModbusTCP(t)
	s.rtu = true
	c := s.client(t)

	regs, err := c.ReadRegisters(1, 3, 0x16, 3)
	if err != nil {
		t.Fatal(err)
	}
	if regs[0] != 0x16+1000 || regs[2] != 0x18+1000 {
		t.Errorf("regs = %v", regs)
	}
	if err := c.WriteRegister(1, 0xB9, 5); err != nil {
		t.Fatal(err)
	}
	if err := c.WriteRegisters(1, 0x100, []uint16{5, 4}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ReadRegisters(1, 4, 9999, 1); err == nil {
		t.Error("an exception response must be an error")
	}
	// The exception's 5-byte frame was read whole: the next read still works.
	if _, err := c.ReadRegisters(1, 4, 0, 1); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.written[0xB9] != 5 || s.written[0x100] != 5 || s.written[0x101] != 4 {
		t.Errorf("written = %v", s.written)
	}
	if s.conns != 1 {
		t.Errorf("%d connections, want the one kept open", s.conns)
	}
}

func TestRTUOverTCPClientRejectsABadCRC(t *testing.T) {
	s := startFakeModbusTCP(t)
	s.rtu, s.badCRC = true, true
	if _, err := s.client(t).ReadRegisters(1, 3, 0, 2); err == nil {
		t.Fatal("a response with a bad CRC must be an error")
	}
}

func TestRTUOverTCPClientReconnectsAfterTheServerCloses(t *testing.T) {
	s := startFakeModbusTCP(t)
	s.rtu, s.dropAfter = true, 1
	tcpRetryDelay = time.Millisecond
	c := s.client(t)
	for i := 0; i < 3; i++ {
		if _, err := c.ReadRegisters(1, 4, uint16(i), 1); err != nil {
			t.Fatalf("read %d: %v", i, err)
		}
	}
}
