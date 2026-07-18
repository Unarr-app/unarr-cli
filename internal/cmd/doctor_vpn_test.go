package cmd

import (
	"errors"
	"strings"
	"testing"
)

// TestEvaluateVPNDoctor covers the pure doctor decision in the check() convention:
// non-nil error → FAIL, a leading '!' → WARN, otherwise PASS.
func TestEvaluateVPNDoctor(t *testing.T) {
	tests := []struct {
		name     string
		in       vpnDoctorInput
		wantFail bool
		wantWarn bool
	}{
		{"not required, off", vpnDoctorInput{}, false, false},
		{"not required, configured", vpnDoctorInput{enabled: true}, false, false},
		{"required but VPN off", vpnDoctorInput{required: true}, true, false},
		{"required, daemon down", vpnDoctorInput{required: true, enabled: true}, false, true},
		{"required, tunnel blocking", vpnDoctorInput{required: true, enabled: true, daemonAlive: true, vpnBlocking: true}, true, false},
		{"required, tunnel active", vpnDoctorInput{required: true, enabled: true, daemonAlive: true, vpnActive: true}, false, false},
		{"required, alive, coming up", vpnDoctorInput{required: true, hasConfigFile: true, daemonAlive: true}, false, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			msg, err := evaluateVPNDoctor(tt.in)
			if (err != nil) != tt.wantFail {
				t.Errorf("err = %v, wantFail %v", err, tt.wantFail)
			}
			if tt.wantFail && !errors.Is(err, errVPNKillSwitch) {
				t.Errorf("fail case should return errVPNKillSwitch, got %v", err)
			}
			if gotWarn := strings.HasPrefix(msg, "!"); gotWarn != tt.wantWarn {
				t.Errorf("warn(msg=%q) = %v, wantWarn %v", msg, gotWarn, tt.wantWarn)
			}
		})
	}
}
