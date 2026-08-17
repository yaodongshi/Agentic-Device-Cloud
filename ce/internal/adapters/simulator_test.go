package adapters

// modbusSimulator is a purpose-built Modbus TCP server backing the C1.6
// certification suite. It implements exactly the function codes the
// adapter exercises (FC 1/2 read bits, FC 3/4 read registers, FC 5/6
// write single), with correct MBAP framing and exception responses
// (illegal function / illegal data address / illegal data value).
//
// Why hand-rolled instead of a library server:
//   - goburrow/modbus ships no server implementation (client only), and a
//     third-party server dependency would exist purely for tests;
//   - ~200 lines of net + encoding/binary give deterministic control over
//     exception codes, connection teardown (the health-after-disconnect
//     cert case) and concurrency, which is exactly what the certification
//     matrix (discover/read/write/error/health/risk) needs;
//   - it keeps the suite hermetic: the simulator listens on 127.0.0.1:0,
//     so there are no port collisions and no external processes.

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/goburrow/modbus"
)

// simAddrSpace is the simulated address space: addresses 0..999 exist,
// anything at or above 1000 answers exception code 2 (illegal data
// address), which the cert suite uses for its error cases.
const simAddrSpace = 1000

// mbapHeaderSize is the 7-byte Modbus TCP header: transaction id (2),
// protocol id (2), length (2), unit id (1).
const mbapHeaderSize = 7

// modbusSimulator holds the simulated device state. All map access goes
// through mu; the listener is immutable after construction.
type modbusSimulator struct {
	ln net.Listener

	mu       sync.Mutex
	holding  map[uint16]uint16
	input    map[uint16]uint16
	coils    map[uint16]bool
	discrete map[uint16]bool
	conns    map[net.Conn]struct{}
	closed   bool

	once sync.Once
	wg   sync.WaitGroup
}

// newModbusSimulator starts a simulator on an ephemeral local port and
// registers its cleanup.
func newModbusSimulator(t *testing.T) *modbusSimulator {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("modbus simulator: listen: %v", err)
	}
	s := &modbusSimulator{
		ln:       ln,
		holding:  make(map[uint16]uint16),
		input:    make(map[uint16]uint16),
		coils:    make(map[uint16]bool),
		discrete: make(map[uint16]bool),
		conns:    make(map[net.Conn]struct{}),
	}
	s.wg.Add(1)
	go s.acceptLoop()
	t.Cleanup(s.Close)
	return s
}

// Addr returns the host:port the simulator listens on.
func (s *modbusSimulator) Addr() string {
	return s.ln.Addr().String()
}

// HostPort returns the host and port the simulator listens on, for
// building adapter configs.
func (s *modbusSimulator) HostPort() (string, int) {
	host, portStr, err := net.SplitHostPort(s.ln.Addr().String())
	if err != nil {
		panic(fmt.Sprintf("modbus simulator: split addr: %v", err))
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		panic(fmt.Sprintf("modbus simulator: parse port: %v", err))
	}
	return host, port
}

// Close stops the simulator: it stops accepting, closes every open
// connection and waits for the accept loop. It is idempotent.
func (s *modbusSimulator) Close() {
	s.once.Do(func() {
		s.mu.Lock()
		s.closed = true
		conns := make([]net.Conn, 0, len(s.conns))
		for c := range s.conns {
			conns = append(conns, c)
		}
		s.mu.Unlock()
		_ = s.ln.Close()
		for _, c := range conns {
			_ = c.Close()
		}
		s.wg.Wait()
	})
}

// SetHolding / SetInput / SetCoil / SetDiscrete preload simulated state.
func (s *modbusSimulator) SetHolding(addr uint16, v uint16) {
	s.mu.Lock()
	s.holding[addr] = v
	s.mu.Unlock()
}

func (s *modbusSimulator) SetInput(addr uint16, v uint16) {
	s.mu.Lock()
	s.input[addr] = v
	s.mu.Unlock()
}

func (s *modbusSimulator) SetCoil(addr uint16, on bool) {
	s.mu.Lock()
	s.coils[addr] = on
	s.mu.Unlock()
}

func (s *modbusSimulator) SetDiscrete(addr uint16, on bool) {
	s.mu.Lock()
	s.discrete[addr] = on
	s.mu.Unlock()
}

func (s *modbusSimulator) acceptLoop() {
	defer s.wg.Done()
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return // listener closed
		}
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			_ = conn.Close()
			return
		}
		s.conns[conn] = struct{}{}
		s.mu.Unlock()
		go s.serveConn(conn)
	}
}

// serveConn answers sequential Modbus requests on one connection until
// the client goes away or the simulator closes.
func (s *modbusSimulator) serveConn(conn net.Conn) {
	defer func() {
		_ = conn.Close()
		s.mu.Lock()
		delete(s.conns, conn)
		s.mu.Unlock()
	}()
	for {
		if err := s.serveRequest(conn); err != nil {
			return
		}
	}
}

// serveRequest reads one MBAP frame, dispatches to handle and writes the
// response frame. Any framing error tears the connection down.
func (s *modbusSimulator) serveRequest(conn net.Conn) error {
	var hdr [mbapHeaderSize]byte
	if _, err := io.ReadFull(conn, hdr[:]); err != nil {
		return err
	}
	length := int(binary.BigEndian.Uint16(hdr[4:6]))
	if length < 2 || length > 254 {
		return fmt.Errorf("modbus simulator: bad MBAP length %d", length)
	}
	body := make([]byte, length-1)
	if _, err := io.ReadFull(conn, body); err != nil {
		return err
	}
	pdu := s.handle(body[0], body[1:])
	out := make([]byte, 0, mbapHeaderSize+len(pdu))
	out = append(out, hdr[:4]...) // echo transaction and protocol id
	// MBAP length counts the unit id byte plus the response PDU.
	out = append(out, byte((len(pdu)+1)>>8), byte(len(pdu)+1))
	out = append(out, hdr[6]) // echo unit id
	out = append(out, pdu...)
	_, err := conn.Write(out)
	return err
}

// handle dispatches one PDU and returns the response PDU (function code
// echo, or fc|0x80 plus exception code).
func (s *modbusSimulator) handle(fc byte, data []byte) []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch fc {
	case 1: // read coils
		return s.readBits(fc, data, s.coils)
	case 2: // read discrete inputs
		return s.readBits(fc, data, s.discrete)
	case 3: // read holding registers
		return s.readRegisters(fc, data, s.holding)
	case 4: // read input registers
		return s.readRegisters(fc, data, s.input)
	case 5: // write single coil
		if len(data) != 4 {
			return exception(fc, modbus.ExceptionCodeIllegalDataValue)
		}
		addr := binary.BigEndian.Uint16(data[0:2])
		if addr >= simAddrSpace {
			return exception(fc, modbus.ExceptionCodeIllegalDataAddress)
		}
		switch binary.BigEndian.Uint16(data[2:4]) {
		case 0x0000:
			s.coils[addr] = false
		case 0xFF00:
			s.coils[addr] = true
		default:
			return exception(fc, modbus.ExceptionCodeIllegalDataValue)
		}
		return append([]byte{fc}, data...)
	case 6: // write single register
		if len(data) != 4 {
			return exception(fc, modbus.ExceptionCodeIllegalDataValue)
		}
		addr := binary.BigEndian.Uint16(data[0:2])
		if addr >= simAddrSpace {
			return exception(fc, modbus.ExceptionCodeIllegalDataAddress)
		}
		s.holding[addr] = binary.BigEndian.Uint16(data[2:4])
		return append([]byte{fc}, data...)
	default:
		return exception(fc, modbus.ExceptionCodeIllegalFunction)
	}
}

// readBits answers FC 1/2: quantity packed into ceil(qty/8) bytes.
func (s *modbusSimulator) readBits(fc byte, data []byte, m map[uint16]bool) []byte {
	if len(data) != 4 {
		return exception(fc, modbus.ExceptionCodeIllegalDataValue)
	}
	addr := binary.BigEndian.Uint16(data[0:2])
	qty := binary.BigEndian.Uint16(data[2:4])
	if qty == 0 || qty > 2000 {
		return exception(fc, modbus.ExceptionCodeIllegalDataValue)
	}
	if int(addr)+int(qty) > simAddrSpace {
		return exception(fc, modbus.ExceptionCodeIllegalDataAddress)
	}
	pdu := make([]byte, 2+int((qty+7)/8))
	pdu[0] = fc
	pdu[1] = byte((qty + 7) / 8)
	for i := 0; i < int(qty); i++ {
		if m[uint16(int(addr)+i)] {
			pdu[2+i/8] |= 1 << uint(i%8)
		}
	}
	return pdu
}

// readRegisters answers FC 3/4: quantity registers, big-endian.
func (s *modbusSimulator) readRegisters(fc byte, data []byte, m map[uint16]uint16) []byte {
	if len(data) != 4 {
		return exception(fc, modbus.ExceptionCodeIllegalDataValue)
	}
	addr := binary.BigEndian.Uint16(data[0:2])
	qty := binary.BigEndian.Uint16(data[2:4])
	if qty == 0 || qty > 125 {
		return exception(fc, modbus.ExceptionCodeIllegalDataValue)
	}
	if int(addr)+int(qty) > simAddrSpace {
		return exception(fc, modbus.ExceptionCodeIllegalDataAddress)
	}
	pdu := make([]byte, 2+int(qty)*2)
	pdu[0] = fc
	pdu[1] = byte(qty * 2)
	for i := 0; i < int(qty); i++ {
		v := m[uint16(int(addr)+i)]
		pdu[2+i*2] = byte(v >> 8)
		pdu[3+i*2] = byte(v)
	}
	return pdu
}

// exception builds a Modbus exception response PDU.
func exception(fc, code byte) []byte {
	return []byte{fc | 0x80, code}
}

// TestSimulatorProtocol pins the simulator's wire behavior with the raw
// goburrow client: read, write round-trip and exception responses. The
// rest of the suite exercises the simulator through the adapter itself.
func TestSimulatorProtocol(t *testing.T) {
	sim := newModbusSimulator(t)
	sim.SetHolding(7, 0x0ABC)
	sim.SetCoil(3, true)

	handler := modbus.NewTCPClientHandler(sim.Addr())
	handler.SlaveId = 1
	handler.IdleTimeout = 0
	handler.Timeout = 2 * time.Second
	client := modbus.NewClient(handler)
	t.Cleanup(func() { _ = handler.Close() })

	b, err := client.ReadHoldingRegisters(7, 1)
	if err != nil {
		t.Fatalf("read holding: %v", err)
	}
	if got := binary.BigEndian.Uint16(b); got != 0x0ABC {
		t.Fatalf("read holding: got %#x want %#x", got, 0x0ABC)
	}

	if _, err := client.WriteSingleRegister(7, 0x1234); err != nil {
		t.Fatalf("write holding: %v", err)
	}
	b, err = client.ReadHoldingRegisters(7, 1)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if got := binary.BigEndian.Uint16(b); got != 0x1234 {
		t.Fatalf("read back: got %#x want %#x", got, 0x1234)
	}

	b, err = client.ReadCoils(3, 1)
	if err != nil {
		t.Fatalf("read coils: %v", err)
	}
	if b[0]&0x01 != 0x01 {
		t.Fatalf("read coils: got %#x want bit 0 set", b[0])
	}

	if _, err := client.ReadHoldingRegisters(5000, 1); err == nil {
		t.Fatal("read at 5000: want exception, got nil")
	} else {
		var me *modbus.ModbusError
		if !errors.As(err, &me) || me.ExceptionCode != modbus.ExceptionCodeIllegalDataAddress {
			t.Fatalf("read at 5000: want exception code 2, got %v", err)
		}
	}
}
