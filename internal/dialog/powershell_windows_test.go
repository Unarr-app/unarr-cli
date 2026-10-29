package dialog

import (
	"os/exec"
	"strings"
	"testing"
)

// Real PowerShell round-trip: a quote PowerShell treats as a string delimiter
// that escapePowerShell missed would end the literal early and break the
// script (a folder named "Bob’s Films" did).
func TestEscapePowerShellRoundTripsThroughRealPowerShell(t *testing.T) {
	in := "D:\\Bob’s ‘Films’ ‚x‛ it's 下载"
	script := "[Console]::OutputEncoding=[Text.UTF8Encoding]::new($false);[Console]::Out.Write('" + escapePowerShell(in) + "')"
	out, err := exec.Command("powershell", "-NoProfile", "-Command", script).CombinedOutput()
	if err != nil {
		t.Fatalf("powershell failed: %v\n%s", err, out)
	}
	if got := strings.TrimSpace(string(out)); got != in {
		t.Errorf("round-trip = %q, want %q", got, in)
	}
}
