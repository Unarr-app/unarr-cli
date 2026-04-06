//go:build manual

package engine

import (
	"context"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/huin/goupnp/dcps/internetgateway2"
)

// TestUPnPLive is a manual integration test that requires a real router with UPnP/NAT-PMP.
// Run with: go test -tags manual -run TestUPnPLive -v ./internal/engine/
func TestUPnPLive(t *testing.T) {
	fmt.Println("=== UPnP/NAT-PMP Live Test ===")

	start := time.Now()
	mapping, err := SetupUPnP(54321)
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("Port mapping FAILED after %s: %v", elapsed, err)
	}

	fmt.Printf("✅ SUCCESS in %s (protocol: %s)\n", elapsed, mapping.protocol)
	fmt.Printf("   External IP:   %s\n", mapping.ExternalIP)
	fmt.Printf("   External Port: %d\n", mapping.ExternalPort)
	fmt.Printf("   Internal Port: %d\n", mapping.InternalPort)

	// Verify the port is actually mapped by listening and checking
	listener, err := net.Listen("tcp", fmt.Sprintf(":%d", mapping.InternalPort))
	if err != nil {
		t.Logf("⚠️  Could not listen on internal port %d: %v", mapping.InternalPort, err)
	} else {
		listener.Close()
		fmt.Printf("   ✅ Internal port %d is available for listening\n", mapping.InternalPort)
	}

	// Cleanup
	mapping.Remove()
	fmt.Println("Port mapping removed.")
}

// TestNATPMPDirect tests NAT-PMP protocol directly against the gateway.
// Run with: go test -tags manual -run TestNATPMPDirect -v ./internal/engine/
func TestNATPMPDirect(t *testing.T) {
	fmt.Println("=== NAT-PMP Direct Test ===")

	gateway := defaultGateway()
	if gateway == "" {
		t.Fatal("Could not determine default gateway")
	}
	fmt.Printf("Gateway: %s\n\n", gateway)

	conn, err := net.DialTimeout("udp4", gateway+":5351", 3*time.Second)
	if err != nil {
		t.Fatalf("Cannot connect to NAT-PMP: %v", err)
	}
	defer conn.Close()

	// 1. External IP
	fmt.Print("External IP via NAT-PMP: ")
	extIP := natpmpExternalIP(conn)
	if extIP == "" {
		fmt.Println("(empty — router may not report it)")
	} else {
		fmt.Println(extIP)
	}

	// 2. TCP mapping
	fmt.Print("TCP mapping 54321→54321: ")
	extPort, lifetime, err := natpmpMapPort(conn, 2, 54321, 54321, 120)
	if err != nil {
		t.Fatalf("FAILED: %v", err)
	}
	fmt.Printf("✅ external=%d lifetime=%ds\n", extPort, lifetime)

	// 3. Cleanup
	fmt.Print("Deleting mapping: ")
	_, _, err = natpmpMapPort(conn, 2, 54321, 0, 0)
	if err != nil {
		fmt.Printf("FAILED: %v\n", err)
	} else {
		fmt.Println("OK")
	}
}

// TestUPnPSOAPDirect tests UPnP-IGD SOAP directly (for debugging routers where NAT-PMP isn't available).
// Run with: go test -tags manual -run TestUPnPSOAPDirect -v ./internal/engine/
func TestUPnPSOAPDirect(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	fmt.Println("=== UPnP-IGD SOAP Direct Test ===")
	fmt.Println()

	// Try WANIPConnection1
	fmt.Print("Discovering WANIPConnection1... ")
	clients, errs, err := internetgateway2.NewWANIPConnection1ClientsCtx(ctx)
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	fmt.Printf("%d client(s), %d error(s)\n", len(clients), len(errs))
	for _, e := range errs {
		fmt.Printf("  err: %v\n", e)
	}
	if len(clients) == 0 {
		t.Fatal("No WANIPConnection1 clients found")
	}

	client := clients[0]
	fmt.Printf("  Device: %s\n", client.ServiceClient.RootDevice.Device.FriendlyName)

	// GetExternalIPAddress
	extIP, err := client.GetExternalIPAddress()
	fmt.Printf("  External IP: %q (err: %v)\n", extIP, err)

	// Try AddPortMapping
	host := client.ServiceClient.RootDevice.URLBase.Host
	localIP := localIPFor(host)
	fmt.Printf("  Local IP: %s\n\n", localIP)

	fmt.Print("AddPortMapping TCP 54321→54321: ")
	err = client.AddPortMapping("", 54321, "TCP", 54321, localIP, true, "unarr-test", 120)
	if err != nil {
		fmt.Printf("FAILED: %v\n", err)
		fmt.Println("\n⚠️  UPnP SOAP AddPortMapping fails on this router. NAT-PMP should work as fallback.")
	} else {
		fmt.Println("OK")
		client.DeletePortMapping("", 54321, "TCP")
		fmt.Println("Mapping deleted.")
	}
}
