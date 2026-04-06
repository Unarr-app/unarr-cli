//go:build manual

package engine

import (
	"context"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/huin/goupnp"
	"github.com/huin/goupnp/dcps/internetgateway2"
)

// TestUPnPDebug performs detailed UPnP discovery diagnostics.
// Run with: go test -tags manual -run TestUPnPDebug -v ./internal/engine/
func TestUPnPDebug(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	fmt.Println("=== UPnP Debug Diagnostics ===")
	fmt.Println()

	// 1. Check network interfaces
	fmt.Println("--- Network Interfaces ---")
	ifaces, _ := net.Interfaces()
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, _ := iface.Addrs()
		for _, addr := range addrs {
			fmt.Printf("  %s: %s (flags: %s)\n", iface.Name, addr, iface.Flags)
		}
	}
	fmt.Println()

	// 2. Raw SSDP discovery — search for ALL UPnP root devices
	fmt.Println("--- Raw SSDP Discovery (all root devices) ---")
	devices, err := goupnp.DiscoverDevicesCtx(ctx, "upnp:rootdevice")
	if err != nil {
		fmt.Printf("  Error: %v\n", err)
	} else {
		fmt.Printf("  Found %d root device(s)\n", len(devices))
		for i, dev := range devices {
			if dev.Err != nil {
				fmt.Printf("  [%d] Error: %v\n", i, dev.Err)
				continue
			}
			rd := dev.Root
			fmt.Printf("  [%d] %s — %s (%s)\n", i, rd.Device.FriendlyName, rd.Device.DeviceType, rd.URLBase.String())
			// List services
			for _, svc := range rd.Device.Services {
				fmt.Printf("       Service: %s\n", svc.ServiceType)
			}
			// List sub-devices
			for _, sub := range rd.Device.Devices {
				fmt.Printf("       SubDevice: %s — %s\n", sub.FriendlyName, sub.DeviceType)
				for _, svc := range sub.Services {
					fmt.Printf("           Service: %s\n", svc.ServiceType)
				}
				for _, sub2 := range sub.Devices {
					fmt.Printf("           SubDevice: %s — %s\n", sub2.FriendlyName, sub2.DeviceType)
					for _, svc := range sub2.Services {
						fmt.Printf("               Service: %s\n", svc.ServiceType)
					}
				}
			}
		}
	}
	fmt.Println()

	// 3. Try specific IGD service types
	fmt.Println("--- IGD Service Discovery ---")

	fmt.Print("  WANIPConnection2: ")
	c2, errs2, err2 := internetgateway2.NewWANIPConnection2ClientsCtx(ctx)
	if err2 != nil {
		fmt.Printf("error: %v\n", err2)
	} else {
		fmt.Printf("%d client(s), %d error(s)\n", len(c2), len(errs2))
		for _, e := range errs2 {
			fmt.Printf("    err: %v\n", e)
		}
		for _, c := range c2 {
			ip, err := c.GetExternalIPAddress()
			fmt.Printf("    device=%s external_ip=%s err=%v\n",
				c.ServiceClient.RootDevice.Device.FriendlyName, ip, err)
		}
	}

	fmt.Print("  WANIPConnection1: ")
	c1, errs1, err1 := internetgateway2.NewWANIPConnection1ClientsCtx(ctx)
	if err1 != nil {
		fmt.Printf("error: %v\n", err1)
	} else {
		fmt.Printf("%d client(s), %d error(s)\n", len(c1), len(errs1))
		for _, e := range errs1 {
			fmt.Printf("    err: %v\n", e)
		}
		for _, c := range c1 {
			ip, err := c.GetExternalIPAddress()
			fmt.Printf("    device=%s external_ip=%s err=%v\n",
				c.ServiceClient.RootDevice.Device.FriendlyName, ip, err)
		}
	}

	fmt.Print("  WANPPPConnection1: ")
	cp, errsp, errp := internetgateway2.NewWANPPPConnection1ClientsCtx(ctx)
	if errp != nil {
		fmt.Printf("error: %v\n", errp)
	} else {
		fmt.Printf("%d client(s), %d error(s)\n", len(cp), len(errsp))
		for _, e := range errsp {
			fmt.Printf("    err: %v\n", e)
		}
		for _, c := range cp {
			ip, err := c.GetExternalIPAddress()
			fmt.Printf("    device=%s external_ip=%s err=%v\n",
				c.ServiceClient.RootDevice.Device.FriendlyName, ip, err)
		}
	}

	fmt.Println()
	fmt.Println("=== Done ===")
}
