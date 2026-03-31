package engine

import (
	"fmt"
	"log"
	"time"

	alog "github.com/anacrolix/log"
	"github.com/anacrolix/upnp"
)

// UPnPMapping represents an active port mapping on the router.
type UPnPMapping struct {
	ExternalIP   string
	ExternalPort int
	InternalPort int
	device       upnp.Device
}

// SetupUPnP discovers the gateway, maps the port, and gets the public IP.
// Returns nil if UPnP is not available or fails.
func SetupUPnP(internalPort int) (*UPnPMapping, error) {
	log.Println("stream: discovering UPnP gateway (10s timeout)...")
	devices := upnp.Discover(0, 10*time.Second, alog.Logger{})
	if len(devices) == 0 {
		return nil, fmt.Errorf("no UPnP devices found (is UPnP enabled on your router?)")
	}

	log.Printf("stream: found %d UPnP device(s), using %s", len(devices), devices[0].ID())
	device := devices[0]

	// Get public IP
	externalIP, err := device.GetExternalIPAddress()
	if err != nil {
		return nil, fmt.Errorf("get external IP: %w", err)
	}
	log.Printf("stream: public IP via UPnP: %s", externalIP)

	// Map port (same internal/external, 2h lease)
	mappedPort, err := device.AddPortMapping(upnp.TCP, internalPort, internalPort, "unarr stream", 2*time.Hour)
	if err != nil {
		return nil, fmt.Errorf("add port mapping %d: %w", internalPort, err)
	}

	log.Printf("stream: UPnP port mapped %s:%d -> local:%d (2h lease)", externalIP, mappedPort, internalPort)
	return &UPnPMapping{
		ExternalIP:   externalIP.String(),
		ExternalPort: mappedPort,
		InternalPort: internalPort,
		device:       device,
	}, nil
}

// Remove deletes the port mapping from the router.
func (m *UPnPMapping) Remove() {
	if m == nil || m.device == nil {
		return
	}
	if err := m.device.DeletePortMapping(upnp.TCP, m.ExternalPort); err != nil {
		log.Printf("stream: failed to remove UPnP mapping: %v", err)
	} else {
		log.Printf("stream: removed UPnP mapping for port %d", m.ExternalPort)
	}
}
