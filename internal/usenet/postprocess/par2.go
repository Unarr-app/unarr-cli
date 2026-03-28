package postprocess

import (
	"fmt"
	"log"
	"os/exec"
	"strings"
)

// Par2Available checks if par2cmdline is installed.
func Par2Available() bool {
	_, err := exec.LookPath("par2")
	return err == nil
}

// Par2Verify verifies files using a par2 file.
// Returns nil if verification passes, error otherwise.
func Par2Verify(par2File string) error {
	if !Par2Available() {
		log.Printf("[usenet] par2 not installed, skipping verification")
		return nil
	}

	cmd := exec.Command("par2", "verify", par2File)
	output, err := cmd.CombinedOutput()
	if err != nil {
		outStr := string(output)
		// Check if repair is possible
		if strings.Contains(outStr, "Repair is possible") {
			return &Par2RepairableError{Par2File: par2File}
		}
		if strings.Contains(outStr, "Repair is not possible") {
			return fmt.Errorf("par2: verification failed and repair not possible:\n%s", outStr)
		}
		return fmt.Errorf("par2 verify: %w\n%s", err, outStr)
	}

	log.Printf("[usenet] par2: verification OK")
	return nil
}

// Par2Repair attempts to repair files using par2 parity data.
func Par2Repair(par2File string) error {
	if !Par2Available() {
		return fmt.Errorf("par2 not installed")
	}

	cmd := exec.Command("par2", "repair", par2File)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("par2 repair: %w\n%s", err, output)
	}

	log.Printf("[usenet] par2: repair successful")
	return nil
}

// Par2RepairableError indicates verification failed but repair is possible.
type Par2RepairableError struct {
	Par2File string
}

func (e *Par2RepairableError) Error() string {
	return fmt.Sprintf("par2: verification failed, repair possible: %s", e.Par2File)
}
