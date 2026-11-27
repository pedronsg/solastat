package modbus

import (
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"strconv"
	"sync"
	"time"

	"github.com/OrbitOS-org/orbit-os-sdk-go/v26/logger"
)

// Modbus TCP: the inverter's own Ethernet/Wi-Fi Modbus server (port 502 by
// default), for inverters reached over the network without a Solarman
// dongle. A request is the same PDU as Modbus RTU's — function code and
// data — without the slave address and CRC, behind a 7-byte MBAP header:
//
//	[0:2)  transaction id, echoed by the response
//	[2:4)  protocol id = 0
//	[4:6)  length of what follows (unit id + PDU)
//	[6]    unit id (the profile's slave address)
//	[7:.)  the PDU
//
// The client builds its requests and parses its responses with the RTU
// helpers (buildRequest, parseResponse…): it strips a built RTU frame down
// to its PDU, and puts the unit id and a CRC back on a response, so the
// same validation (exceptions, lengths, ids) applies to both transports.
//
// Modbus RTU over TCP: a serial gateway that passes bytes through between a
// TCP socket and the inverter's RS485 line untouched — e.g. a Solarman
// dongle in "Transparency" mode (port 8899), or an Elfin/USR converter. The
// frames are plain RTU, address and CRC included, with no header around
// them; TCPClient speaks it too, when made by NewRTUOverTCPClient.
const (
	tcpDialTimeout = 3 * time.Second
	tcpIOTimeout   = 2 * time.Second
	mbapLen        = 7
)

// tcpRetryDelay is the pause before a transaction's single retry on a fresh
// connection (a var so tests can shorten it).
var tcpRetryDelay = 250 * time.Millisecond

// TCPClient implements Access over Modbus TCP. Like SolarmanClient, it
// keeps one connection open for every transaction (a poll is a dozen
// requests every few seconds), closes it on any error so a late response
// can never be taken for the next one's, and retries a failed transaction
// once on a fresh connection.
type TCPClient struct {
	addr string
	rtu  bool   // RTU frames as they are, instead of MBAP
	kind string // for logs and errors: "modbus tcp" or "modbus rtu-over-tcp"

	mu   sync.Mutex // serializes transactions and guards conn and tid
	tid  uint16
	conn net.Conn // nil until the first transaction, and after any error
}

// NewTCPClient returns a client for the Modbus TCP server at host:port.
func NewTCPClient(host string, port int) *TCPClient {
	return &TCPClient{addr: net.JoinHostPort(host, strconv.Itoa(port)), kind: "modbus tcp"}
}

// NewRTUOverTCPClient returns a client for the RTU-over-TCP gateway at
// host:port.
func NewRTUOverTCPClient(host string, port int) *TCPClient {
	return &TCPClient{addr: net.JoinHostPort(host, strconv.Itoa(port)), rtu: true, kind: "modbus rtu-over-tcp"}
}

// Stop closes the open connection, if any.
func (c *TCPClient) Stop() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closeConn()
}

func (c *TCPClient) closeConn() {
	if c.conn != nil {
		_ = c.conn.Close()
		c.conn = nil
	}
}

func (c *TCPClient) ReadRegisters(slaveID, fc byte, address, count uint16) ([]uint16, error) {
	resp, err := c.transact(buildRequest(slaveID, fc, address, count))
	if err != nil {
		return nil, err
	}
	return parseResponse(resp, slaveID, fc, count)
}

func (c *TCPClient) WriteRegister(slaveID byte, address, value uint16) error {
	resp, err := c.transact(buildRequest(slaveID, 6, address, value))
	if err != nil {
		return err
	}
	return parseWriteResponse(resp, slaveID, address, value)
}

func (c *TCPClient) WriteRegisters(slaveID byte, address uint16, values []uint16) error {
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

// transact sends one request — an RTU frame, sent as its unit id and PDU —
// and returns the response as an RTU frame (unit id, PDU, CRC). On failure
// it retries once, on a fresh connection, after tcpRetryDelay (a retried
// write just writes the same value again, which is harmless).
func (c *TCPClient) transact(rtuReq []byte) ([]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	resp, err := c.transactOnce(rtuReq)
	if err == nil {
		return resp, nil
	}
	logger.Debugf(logTag, "%s %s: %v — retrying on a new connection", c.kind, c.addr, err)
	time.Sleep(tcpRetryDelay)
	return c.transactOnce(rtuReq)
}

// transactOnce runs one request/response on the open connection, dialling
// one first if needed. Any error closes the connection.
func (c *TCPClient) transactOnce(rtuReq []byte) (resp []byte, err error) {
	defer func() {
		if err != nil {
			c.closeConn()
		}
	}()

	if c.conn == nil {
		conn, err := net.DialTimeout("tcp", c.addr, tcpDialTimeout)
		if err != nil {
			return nil, fmt.Errorf("dial %s %s: %w", c.kind, c.addr, err)
		}
		c.conn = conn
	}
	_ = c.conn.SetDeadline(time.Now().Add(tcpIOTimeout))

	frame := rtuReq
	if !c.rtu {
		c.tid++
		frame = mbapEncode(c.tid, rtuReq)
	}
	logger.Debugf(logTag, "%s %s: writing frame: %s", c.kind, c.addr, toHex(frame))
	if _, err := c.conn.Write(frame); err != nil {
		return nil, fmt.Errorf("write %s frame: %w", c.kind, err)
	}

	if c.rtu {
		resp, err = rtuRead(c.conn, rtuReq[1])
	} else {
		resp, err = mbapRead(c.conn, c.tid)
	}
	if err != nil {
		return nil, fmt.Errorf("read %s frame: %w", c.kind, err)
	}
	logger.Debugf(logTag, "%s %s: got frame (as rtu): %s", c.kind, c.addr, toHex(resp))
	return resp, nil
}

// rtuRead reads one RTU response to a request with function code fc. RTU
// has no length field, so the size comes from the frame itself: an
// exception is 5 bytes, a read 5 plus its byte count, a write's echo 8.
// The CRC is checked by the RTU parsers.
func rtuRead(r io.Reader, fc byte) ([]byte, error) {
	frame := make([]byte, 3, 260)
	if _, err := io.ReadFull(r, frame); err != nil {
		return nil, err
	}
	n := 8
	switch {
	case frame[1]&0x80 != 0:
		n = 5
	case fc == 3 || fc == 4:
		n = 5 + int(frame[2])
	}
	frame = frame[:n]
	if _, err := io.ReadFull(r, frame[3:]); err != nil {
		return nil, err
	}
	return frame, nil
}

// mbapEncode turns an RTU request frame (unit id, PDU, CRC) into a Modbus
// TCP one: MBAP header, unit id, PDU.
func mbapEncode(tid uint16, rtuReq []byte) []byte {
	unitAndPDU := rtuReq[:len(rtuReq)-2] // drop the CRC
	frame := make([]byte, 6, 6+len(unitAndPDU))
	binary.BigEndian.PutUint16(frame[0:], tid)
	binary.BigEndian.PutUint16(frame[2:], 0) // protocol id
	binary.BigEndian.PutUint16(frame[4:], uint16(len(unitAndPDU)))
	return append(frame, unitAndPDU...)
}

// mbapRead reads one Modbus TCP response for transaction wantTID and
// returns it as an RTU frame — unit id, PDU, and a CRC of its own — for the
// RTU parsers. The header's length field fixes the frame's size.
func mbapRead(r io.Reader, wantTID uint16) ([]byte, error) {
	header := make([]byte, mbapLen)
	if _, err := io.ReadFull(r, header); err != nil {
		return nil, err
	}
	tid := binary.BigEndian.Uint16(header[0:])
	proto := binary.BigEndian.Uint16(header[2:])
	length := int(binary.BigEndian.Uint16(header[4:]))
	if proto != 0 {
		return nil, fmt.Errorf("modbus tcp: protocol id %d, want 0", proto)
	}
	if length < 2 || length > 254 {
		return nil, fmt.Errorf("modbus tcp: bad length %d", length)
	}
	pdu := make([]byte, length-1) // length counts the unit id, already read
	if _, err := io.ReadFull(r, pdu); err != nil {
		return nil, err
	}
	if tid != wantTID {
		return nil, fmt.Errorf("modbus tcp: transaction id %d, want %d", tid, wantTID)
	}
	rtu := append([]byte{header[6]}, pdu...)
	crc := crc16(rtu)
	return append(rtu, byte(crc), byte(crc>>8)), nil
}
