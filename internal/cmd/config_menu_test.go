package cmd

import "testing"

func TestValidateSpeed(t *testing.T) {
	tests := []struct {
		input   string
		wantErr bool
	}{
		{"", false},
		{"0", false},
		{" ", false},
		{"10MB", false},
		{"500KB", false},
		{"1GB", false},
		{"abc", true},
		{"10XB", true},
		{"-5MB", true},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			err := validateSpeed(tt.input)
			if (err != nil) != tt.wantErr {
				t.Errorf("validateSpeed(%q) error = %v, wantErr %v", tt.input, err, tt.wantErr)
			}
		})
	}
}

func TestValidateDuration(t *testing.T) {
	tests := []struct {
		input   string
		wantErr bool
	}{
		{"", false},
		{"30s", false},
		{"1m", false},
		{"5m", false},
		{"1h", false},
		{"2h30m", false},
		{"abc", true},
		{"30", true},
		{"5x", true},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			err := validateDuration(tt.input)
			if (err != nil) != tt.wantErr {
				t.Errorf("validateDuration(%q) error = %v, wantErr %v", tt.input, err, tt.wantErr)
			}
		})
	}
}
