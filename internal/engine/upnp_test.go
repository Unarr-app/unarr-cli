package engine

import (
	"encoding/binary"
	"net"
	"sync"
	"testing"
	"time"
)

// --- Mock NAT-PMP server ---

type mockNATPMPServer struct {
	conn     net.PacketConn
	addr     string
	mu       sync.Mutex
	mappings map[uint16]natpmpMapping // internalPort → mapping
	extIP    net.IP
	epoch    uint32
	closed   chan struct{}
}

type natpmpMapping struct {
	extPort  uint16
	protocol byte // 1=UDP, 2=TCP
	lifetime uint32
}

func newMockNATPMP(extIP string) *mockNATPMPServer {
	conn, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		panic(err)
	}
	s := &mockNATPMPServer{
		conn:     conn,
		addr:     conn.LocalAddr().String(),
		mappings: make(map[uint16]natpmpMapping),
		extIP:    net.ParseIP(extIP).To4(),
		epoch:    1000,
		closed:   make(chan struct{}),
	}
	go s.serve()
	return s
}

func (s *mockNATPMPServer) Close() {
	s.conn.Close()
	<-s.closed
}

func (s *mockNATPMPServer) serve() {
	defer close(s.closed)
	buf := make([]byte, 64)
	for {
		n, addr, err := s.conn.ReadFrom(buf)
		if err != nil {
			return
		}
		if n < 2 {
			continue
		}

		opcode := buf[1]
		var resp []byte

		switch opcode {
		case 0: // External address request
			resp = s.handleExternalAddress()
		case 1, 2: // UDP/TCP mapping
			if n >= 12 {
				resp = s.handleMapping(buf[:n])
			}
		}

		if resp != nil {
			s.conn.WriteTo(resp, addr)
		}
	}
}

func (s *mockNATPMPServer) handleExternalAddress() []byte {
	resp := make([]byte, 12)
	resp[0] = 0   // version
	resp[1] = 128 // opcode 0 + 128
	// result code 0 = success
	binary.BigEndian.PutUint32(resp[4:8], s.epoch)
	copy(resp[8:12], s.extIP)
	return resp
}

func (s *mockNATPMPServer) handleMapping(req []byte) []byte {
	s.mu.Lock()
	defer s.mu.Unlock()

	opcode := req[1]
	intPort := binary.BigEndian.Uint16(req[4:6])
	sugExtPort := binary.BigEndian.Uint16(req[6:8])
	lifetime := binary.BigEndian.Uint32(req[8:12])

	resp := make([]byte, 16)
	resp[0] = 0
	resp[1] = 128 + opcode
	binary.BigEndian.PutUint32(resp[4:8], s.epoch)

	if lifetime == 0 {
		// Delete mapping
		delete(s.mappings, intPort)
		binary.BigEndian.PutUint16(resp[8:10], intPort)
		binary.BigEndian.PutUint16(resp[10:12], 0)
		binary.BigEndian.PutUint32(resp[12:16], 0)
	} else {
		// Create mapping
		extPort := sugExtPort
		if extPort == 0 {
			extPort = intPort
		}
		s.mappings[intPort] = natpmpMapping{
			extPort:  extPort,
			protocol: opcode,
			lifetime: lifetime,
		}
		binary.BigEndian.PutUint16(resp[8:10], intPort)
		binary.BigEndian.PutUint16(resp[10:12], extPort)
		binary.BigEndian.PutUint32(resp[12:16], lifetime)
	}

	return resp
}

// --- Mock UPnP client ---

type mockUPnPClient struct {
	externalIP  string
	externalErr error
	addErr      error
	deleteErr   error
	lastMapping *mockPortMapping
}

type mockPortMapping struct {
	remoteHost  string
	extPort     uint16
	protocol    string
	intPort     uint16
	intClient   string
	enabled     bool
	description string
	lease       uint32
}

func (m *mockUPnPClient) GetExternalIPAddress() (string, error) {
	return m.externalIP, m.externalErr
}

func (m *mockUPnPClient) AddPortMapping(remoteHost string, extPort uint16, proto string, intPort uint16, intClient string, enabled bool, desc string, lease uint32) error {
	if m.addErr != nil {
		return m.addErr
	}
	m.lastMapping = &mockPortMapping{
		remoteHost:  remoteHost,
		extPort:     extPort,
		protocol:    proto,
		intPort:     intPort,
		intClient:   intClient,
		enabled:     enabled,
		description: desc,
		lease:       lease,
	}
	return nil
}

func (m *mockUPnPClient) DeletePortMapping(remoteHost string, extPort uint16, proto string) error {
	return m.deleteErr
}

// --- Tests ---

func TestNATPMPMapAndDelete(t *testing.T) {
	srv := newMockNATPMP("203.0.113.42")
	defer srv.Close()

	conn, err := net.DialTimeout("udp4", srv.addr, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	// Map port
	extPort, lifetime, err := natpmpMapPort(conn, 2, 8080, 8080, 3600)
	if err != nil {
		t.Fatalf("map: %v", err)
	}
	if extPort != 8080 {
		t.Errorf("expected external port 8080, got %d", extPort)
	}
	if lifetime != 3600 {
		t.Errorf("expected lifetime 3600, got %d", lifetime)
	}

	// Verify mapping stored
	srv.mu.Lock()
	m, ok := srv.mappings[8080]
	srv.mu.Unlock()
	if !ok {
		t.Fatal("mapping not stored in server")
	}
	if m.protocol != 2 {
		t.Errorf("expected TCP (2), got %d", m.protocol)
	}

	// Delete
	_, _, err = natpmpMapPort(conn, 2, 8080, 0, 0)
	if err != nil {
		t.Fatalf("delete: %v", err)
	}

	srv.mu.Lock()
	_, ok = srv.mappings[8080]
	srv.mu.Unlock()
	if ok {
		t.Error("mapping should have been deleted")
	}
}

func TestNATPMPExternalIP(t *testing.T) {
	srv := newMockNATPMP("93.184.216.34")
	defer srv.Close()

	conn, err := net.DialTimeout("udp4", srv.addr, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	ip := natpmpExternalIP(conn)
	if ip != "93.184.216.34" {
		t.Errorf("expected 93.184.216.34, got %q", ip)
	}
}

func TestNATPMPExternalIPUnspecified(t *testing.T) {
	srv := newMockNATPMP("0.0.0.0")
	defer srv.Close()

	conn, err := net.DialTimeout("udp4", srv.addr, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	ip := natpmpExternalIP(conn)
	if ip != "" {
		t.Errorf("expected empty for 0.0.0.0, got %q", ip)
	}
}

func TestUPnPSetupMappingSuccess(t *testing.T) {
	mock := &mockUPnPClient{externalIP: "198.51.100.1"}

	mapping, err := setupMapping("192.168.1.1:1900", mock, 9000)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if mapping.ExternalIP != "198.51.100.1" {
		t.Errorf("expected external IP 198.51.100.1, got %s", mapping.ExternalIP)
	}
	if mapping.ExternalPort != 9000 {
		t.Errorf("expected port 9000, got %d", mapping.ExternalPort)
	}
	if mapping.protocol != "upnp" {
		t.Errorf("expected protocol upnp, got %s", mapping.protocol)
	}
	if mock.lastMapping == nil {
		t.Fatal("AddPortMapping not called")
	}
	if mock.lastMapping.protocol != "TCP" {
		t.Errorf("expected TCP, got %s", mock.lastMapping.protocol)
	}
	if !mock.lastMapping.enabled {
		t.Error("expected enabled=true")
	}
}

func TestUPnPSetupMappingAddFails(t *testing.T) {
	mock := &mockUPnPClient{
		externalIP: "198.51.100.1",
		addErr:     net.ErrClosed,
	}

	_, err := setupMapping("192.168.1.1:1900", mock, 9000)
	if err == nil {
		t.Fatal("expected error from AddPortMapping")
	}
}

func TestUPnPSetupMappingEmptyIP(t *testing.T) {
	// When router returns empty IP and public IP fallback also fails
	mock := &mockUPnPClient{externalIP: ""}

	// setupMapping calls publicIPFallback() which requires internet.
	// In unit tests, this may or may not work. We just verify it doesn't panic.
	mapping, err := setupMapping("192.168.1.1:1900", mock, 9000)
	if err != nil {
		// Expected if no internet / public IP fallback fails
		t.Logf("expected failure with empty IP: %v", err)
		return
	}
	// If it succeeded (has internet), verify the mapping is valid
	if mapping.ExternalIP == "" {
		t.Error("mapping should have a non-empty external IP")
	}
}

func TestUPnPMappingRemoveNATPMP(t *testing.T) {
	// Remove() connects to gateway:5351 (standard NAT-PMP port).
	// We can't redirect to a mock easily, but verify it doesn't panic
	// even when the gateway is unreachable.
	mapping := &UPnPMapping{
		ExternalIP:   "203.0.113.42",
		ExternalPort: 8080,
		InternalPort: 8080,
		gateway:      "192.0.2.1", // RFC 5737 TEST-NET — unreachable
		protocol:     "natpmp",
	}
	mapping.Remove() // should not panic, just log the error
}

func TestUPnPMappingRemoveUPnP(t *testing.T) {
	mock := &mockUPnPClient{}
	mapping := &UPnPMapping{
		ExternalPort: 9000,
		protocol:     "upnp",
		client:       mock,
	}
	// Should not panic
	mapping.Remove()
}

func TestUPnPMappingRemoveNil(t *testing.T) {
	var m *UPnPMapping
	m.Remove() // should not panic
}

func TestDefaultGateway(t *testing.T) {
	gw := defaultGateway()
	if gw == "" {
		t.Skip("no network connectivity")
	}
	ip := net.ParseIP(gw)
	if ip == nil {
		t.Errorf("defaultGateway returned invalid IP: %q", gw)
	}
}

func TestLocalIPFor(t *testing.T) {
	ip := localIPFor("192.168.0.1:1900")
	if ip == "0.0.0.0" {
		t.Skip("no route to 192.168.0.1")
	}
	parsed := net.ParseIP(ip)
	if parsed == nil {
		t.Errorf("localIPFor returned invalid IP: %q", ip)
	}
}
