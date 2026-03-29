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

// setupUPnP discovers the gateway, maps the port, and gets the public IP.
// Returns nil if UPnP is not available or fails.
func setupUPnP(internalPort int) (*UPnPMapping, error) {
	devices := upnp.Discover(0, 5*time.Second, alog.Logger{})
	if len(devices) == 0 {
		return nil, fmt.Errorf("no UPnP devices found")
	}

	device := devices[0]

	// Get public IP
	externalIP, err := device.GetExternalIPAddress()
	if err != nil {
		return nil, fmt.Errorf("get external IP: %w", err)
	}

	// Map port (0 = let router choose external port, 2h lease)
	mappedPort, err := device.AddPortMapping(upnp.TCP, internalPort, internalPort, "unarr stream", 2*time.Hour)
	if err != nil {
		return nil, fmt.Errorf("add port mapping: %w", err)
	}

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
