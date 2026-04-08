package engine

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/huin/goupnp/dcps/internetgateway2"
)

// UPnPMapping represents an active port mapping on the router.
type UPnPMapping struct {
	ExternalIP   string
	ExternalPort int
	InternalPort int
	gateway      string     // for NAT-PMP cleanup
	protocol     string     // "natpmp" or "upnp"
	client       upnpClient // for UPnP cleanup (nil if NAT-PMP)
}

// upnpClient abstracts the IGD service methods we need (WANIPConnection or WANPPPConnection).
type upnpClient interface {
	AddPortMapping(
		NewRemoteHost string,
		NewExternalPort uint16,
		NewProtocol string,
		NewInternalPort uint16,
		NewInternalClient string,
		NewEnabled bool,
		NewPortMappingDescription string,
		NewLeaseDuration uint32,
	) error
	DeletePortMapping(
		NewRemoteHost string,
		NewExternalPort uint16,
		NewProtocol string,
	) error
	GetExternalIPAddress() (string, error)
}

// SetupUPnP discovers the gateway, maps the port, and gets the public IP.
// Tries NAT-PMP first (faster, more compatible), falls back to UPnP-IGD SOAP.
func SetupUPnP(internalPort int) (*UPnPMapping, error) {
	log.Println("stream: discovering NAT gateway...")

	gateway := defaultGateway()

	// Try NAT-PMP first (preferred — works on most modern routers including TP-Link)
	if gateway != "" {
		if mapping, err := tryNATPMP(gateway, internalPort); err == nil {
			return mapping, nil
		} else {
			log.Printf("stream: NAT-PMP failed (%v), trying UPnP-IGD...", err)
		}
	}

	// Fall back to UPnP-IGD SOAP
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if mapping, err := tryWANIPConnection2(ctx, internalPort); err == nil {
		return mapping, nil
	}
	if mapping, err := tryWANIPConnection1(ctx, internalPort); err == nil {
		return mapping, nil
	}
	if mapping, err := tryWANPPPConnection1(ctx, internalPort); err == nil {
		return mapping, nil
	}

	return nil, fmt.Errorf("no NAT gateway found (tried NAT-PMP, IGD2, IGD1, PPP)")
}

// --- NAT-PMP implementation (RFC 6886) ---

func tryNATPMP(gateway string, port int) (*UPnPMapping, error) {
	conn, err := net.DialTimeout("udp4", gateway+":5351", 3*time.Second)
	if err != nil {
		return nil, fmt.Errorf("NAT-PMP dial: %w", err)
	}
	defer conn.Close()

	// Map TCP port
	extPort, lifetime, err := natpmpMapPort(conn, 2, uint16(port), uint16(port), 7200)
	if err != nil {
		return nil, fmt.Errorf("NAT-PMP map TCP: %w", err)
	}

	// Get external IP: try NAT-PMP first, fall back to public API
	extIP := natpmpExternalIP(conn)
	if extIP == "" {
		extIP = publicIPFallback()
	}
	if extIP == "" {
		// Clean up the mapping we just created
		if _, _, err := natpmpMapPort(conn, 2, uint16(port), 0, 0); err != nil {
			log.Printf("stream: failed to clean up NAT-PMP mapping after IP failure: %v", err)
		}
		return nil, fmt.Errorf("NAT-PMP: port mapped but could not determine external IP")
	}

	log.Printf("stream: NAT-PMP port mapped %s:%d -> :%d (lease %ds)",
		extIP, extPort, port, lifetime)

	return &UPnPMapping{
		ExternalIP:   extIP,
		ExternalPort: int(extPort),
		InternalPort: port,
		gateway:      gateway,
		protocol:     "natpmp",
	}, nil
}

// natpmpMapPort sends a NAT-PMP mapping request.
// opcode: 1=UDP, 2=TCP. lifetime=0 to delete.
func natpmpMapPort(conn net.Conn, opcode byte, internalPort, suggestedExtPort uint16, lifetime uint32) (extPort uint16, actualLifetime uint32, err error) {
	conn.SetDeadline(time.Now().Add(5 * time.Second))

	req := make([]byte, 12)
	req[0] = 0      // version
	req[1] = opcode // 1=UDP, 2=TCP
	binary.BigEndian.PutUint16(req[4:6], internalPort)
	binary.BigEndian.PutUint16(req[6:8], suggestedExtPort)
	binary.BigEndian.PutUint32(req[8:12], lifetime)

	if _, err := conn.Write(req); err != nil {
		return 0, 0, fmt.Errorf("write: %w", err)
	}

	buf := make([]byte, 16)
	n, err := conn.Read(buf)
	if err != nil {
		return 0, 0, fmt.Errorf("read: %w", err)
	}
	if n < 16 {
		return 0, 0, fmt.Errorf("short response: %d bytes", n)
	}

	resultCode := binary.BigEndian.Uint16(buf[2:4])
	if resultCode != 0 {
		names := map[uint16]string{
			1: "unsupported version", 2: "not authorized",
			3: "network failure", 4: "out of resources", 5: "unsupported opcode",
		}
		name := names[resultCode]
		if name == "" {
			name = "unknown"
		}
		return 0, 0, fmt.Errorf("result %d (%s)", resultCode, name)
	}

	extPort = binary.BigEndian.Uint16(buf[10:12])
	actualLifetime = binary.BigEndian.Uint32(buf[12:16])
	return extPort, actualLifetime, nil
}

// natpmpExternalIP queries the external IP via NAT-PMP (opcode 0).
func natpmpExternalIP(conn net.Conn) string {
	conn.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err := conn.Write([]byte{0, 0}); err != nil {
		return ""
	}
	buf := make([]byte, 12)
	n, err := conn.Read(buf)
	if err != nil || n < 12 {
		return ""
	}
	resultCode := binary.BigEndian.Uint16(buf[2:4])
	if resultCode != 0 {
		return ""
	}
	ip := net.IPv4(buf[8], buf[9], buf[10], buf[11])
	if ip.IsUnspecified() {
		return ""
	}
	return ip.String()
}

// publicIPFallback fetches the external IP from a public API.
func publicIPFallback() string {
	client := &http.Client{Timeout: 5 * time.Second}
	for _, url := range []string{
		"https://api.ipify.org",
		"https://ifconfig.me/ip",
	} {
		resp, err := client.Get(url)
		if err != nil {
			continue
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, 64))
		resp.Body.Close()
		if err != nil || resp.StatusCode != 200 {
			continue
		}
		ip := strings.TrimSpace(string(body))
		if net.ParseIP(ip) != nil {
			return ip
		}
	}
	return ""
}

// --- UPnP-IGD SOAP implementation ---

func tryWANIPConnection2(ctx context.Context, port int) (*UPnPMapping, error) {
	clients, _, err := internetgateway2.NewWANIPConnection2ClientsCtx(ctx)
	if err != nil || len(clients) == 0 {
		return nil, fmt.Errorf("WANIPConnection2: %v (found %d)", err, len(clients))
	}
	return setupMapping(clients[0].ServiceClient.RootDevice.URLBase.Host, &wanIP2Adapter{clients[0]}, port)
}

func tryWANIPConnection1(ctx context.Context, port int) (*UPnPMapping, error) {
	clients, _, err := internetgateway2.NewWANIPConnection1ClientsCtx(ctx)
	if err != nil || len(clients) == 0 {
		return nil, fmt.Errorf("WANIPConnection1: %v (found %d)", err, len(clients))
	}
	return setupMapping(clients[0].ServiceClient.RootDevice.URLBase.Host, &wanIP1Adapter{clients[0]}, port)
}

func tryWANPPPConnection1(ctx context.Context, port int) (*UPnPMapping, error) {
	clients, _, err := internetgateway2.NewWANPPPConnection1ClientsCtx(ctx)
	if err != nil || len(clients) == 0 {
		return nil, fmt.Errorf("WANPPPConnection1: %v (found %d)", err, len(clients))
	}
	return setupMapping(clients[0].ServiceClient.RootDevice.URLBase.Host, &wanPPP1Adapter{clients[0]}, port)
}

func setupMapping(deviceHost string, client upnpClient, internalPort int) (*UPnPMapping, error) {
	externalIP, err := client.GetExternalIPAddress()
	if err != nil {
		return nil, fmt.Errorf("get external IP: %w", err)
	}
	if externalIP == "" {
		externalIP = publicIPFallback()
	}
	if externalIP == "" {
		return nil, fmt.Errorf("could not determine external IP")
	}

	localIP := localIPFor(deviceHost)

	err = client.AddPortMapping(
		"",                   // remote host (empty = any)
		uint16(internalPort), // external port
		"TCP",                // protocol
		uint16(internalPort), // internal port
		localIP,              // internal client IP
		true,                 // enabled
		"unarr stream",       // description
		7200,                 // lease duration (2 hours)
	)
	if err != nil {
		return nil, fmt.Errorf("add port mapping %d: %w", internalPort, err)
	}

	log.Printf("stream: UPnP port mapped %s:%d -> %s:%d (2h lease)", externalIP, internalPort, localIP, internalPort)
	return &UPnPMapping{
		ExternalIP:   externalIP,
		ExternalPort: internalPort,
		InternalPort: internalPort,
		protocol:     "upnp",
		client:       client,
	}, nil
}

// --- Helpers ---

// defaultGateway returns the default gateway IP.
// Reads /proc/net/route on Linux, falls back to assuming .1 on the local subnet.
func defaultGateway() string {
	// Try /proc/net/route first (Linux only, no external dependency)
	if gw := gatewayFromProcRoute(); gw != "" {
		return gw
	}

	// Fallback: assume .1 on the local subnet (works for most home routers)
	conn, err := net.Dial("udp4", "8.8.8.8:80")
	if err != nil {
		return ""
	}
	defer conn.Close()

	ip := conn.LocalAddr().(*net.UDPAddr).IP.To4()
	if ip == nil {
		return ""
	}
	return net.IPv4(ip[0], ip[1], ip[2], 1).String()
}

// gatewayFromProcRoute parses /proc/net/route for the default route gateway.
func gatewayFromProcRoute() string {
	data, err := os.ReadFile("/proc/net/route")
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		// Default route: destination is 00000000
		if fields[1] != "00000000" {
			continue
		}
		// Gateway is field 2 in little-endian hex
		gw, err := fmt.Sscanf(fields[2], "%x", new(uint32))
		if err != nil || gw != 1 {
			continue
		}
		var gwInt uint32
		fmt.Sscanf(fields[2], "%x", &gwInt)
		return fmt.Sprintf("%d.%d.%d.%d",
			gwInt&0xFF, (gwInt>>8)&0xFF, (gwInt>>16)&0xFF, (gwInt>>24)&0xFF)
	}
	return ""
}

// localIPFor returns the local IP that can reach the given host (typically the router).
func localIPFor(host string) string {
	h, _, err := net.SplitHostPort(host)
	if err != nil {
		h = host
	}
	conn, err := net.Dial("udp4", h+":1")
	if err != nil {
		return "0.0.0.0"
	}
	defer conn.Close()
	return conn.LocalAddr().(*net.UDPAddr).IP.String()
}

// Remove deletes the port mapping from the router.
// It runs in a goroutine with a 5-second deadline so it never blocks shutdown.
func (m *UPnPMapping) Remove() {
	if m == nil {
		return
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		switch m.protocol {
		case "natpmp":
			m.removeNATPMP()
		case "upnp":
			m.removeUPnP()
		}
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		// removeNATPMP worst case: 3s dial + 5s natpmpMapPort deadline = 8s.
		// 10s gives enough margin without blocking shutdown indefinitely.
		log.Printf("stream: UPnP/NAT-PMP cleanup timed out after 10s — port %d may remain mapped", m.ExternalPort)
	}
}

func (m *UPnPMapping) removeNATPMP() {
	if m.gateway == "" {
		return
	}
	conn, err := net.DialTimeout("udp4", m.gateway+":5351", 3*time.Second)
	if err != nil {
		log.Printf("stream: failed to connect for NAT-PMP cleanup: %v", err)
		return
	}
	defer conn.Close()

	_, _, err = natpmpMapPort(conn, 2, uint16(m.InternalPort), 0, 0)
	if err != nil {
		log.Printf("stream: failed to remove NAT-PMP mapping: %v", err)
	} else {
		log.Printf("stream: removed NAT-PMP mapping for port %d", m.ExternalPort)
	}
}

func (m *UPnPMapping) removeUPnP() {
	if m.client == nil {
		return
	}
	if err := m.client.DeletePortMapping("", uint16(m.ExternalPort), "TCP"); err != nil {
		log.Printf("stream: failed to remove UPnP mapping: %v", err)
	} else {
		log.Printf("stream: removed UPnP mapping for port %d", m.ExternalPort)
	}
}

// --- Adapters to unify WANIPConnection2, WANIPConnection1, WANPPPConnection1 ---

type wanIP2Adapter struct {
	c *internetgateway2.WANIPConnection2
}

func (a *wanIP2Adapter) AddPortMapping(remoteHost string, extPort uint16, proto string, intPort uint16, intClient string, enabled bool, desc string, lease uint32) error {
	return a.c.AddPortMapping(remoteHost, extPort, proto, intPort, intClient, enabled, desc, lease)
}
func (a *wanIP2Adapter) DeletePortMapping(remoteHost string, extPort uint16, proto string) error {
	return a.c.DeletePortMapping(remoteHost, extPort, proto)
}
func (a *wanIP2Adapter) GetExternalIPAddress() (string, error) {
	return a.c.GetExternalIPAddress()
}

type wanIP1Adapter struct {
	c *internetgateway2.WANIPConnection1
}

func (a *wanIP1Adapter) AddPortMapping(remoteHost string, extPort uint16, proto string, intPort uint16, intClient string, enabled bool, desc string, lease uint32) error {
	return a.c.AddPortMapping(remoteHost, extPort, proto, intPort, intClient, enabled, desc, lease)
}
func (a *wanIP1Adapter) DeletePortMapping(remoteHost string, extPort uint16, proto string) error {
	return a.c.DeletePortMapping(remoteHost, extPort, proto)
}
func (a *wanIP1Adapter) GetExternalIPAddress() (string, error) {
	return a.c.GetExternalIPAddress()
}

type wanPPP1Adapter struct {
	c *internetgateway2.WANPPPConnection1
}

func (a *wanPPP1Adapter) AddPortMapping(remoteHost string, extPort uint16, proto string, intPort uint16, intClient string, enabled bool, desc string, lease uint32) error {
	return a.c.AddPortMapping(remoteHost, extPort, proto, intPort, intClient, enabled, desc, lease)
}
func (a *wanPPP1Adapter) DeletePortMapping(remoteHost string, extPort uint16, proto string) error {
	return a.c.DeletePortMapping(remoteHost, extPort, proto)
}
func (a *wanPPP1Adapter) GetExternalIPAddress() (string, error) {
	return a.c.GetExternalIPAddress()
}
