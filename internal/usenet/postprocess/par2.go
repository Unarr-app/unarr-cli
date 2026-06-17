package postprocess

import (
	"errors"
	"fmt"
	"log"
	"os/exec"
	"strings"
)

// ErrPar2NotInstalled is returned by Par2Verify/Par2Repair when parity data is
// present but the `par2` binary is missing. The caller MUST surface this rather
// than treat it as "verified OK" — a download that shipped parity but could not
// be checked is delivered UNVERIFIED, not verified.
var ErrPar2NotInstalled = errors.New("par2 not installed")

// ErrPar2Unrepairable is returned by Par2Verify when parity confirms the data is
// damaged AND par2 reports repair is not possible — the file is definitively
// corrupt (distinct from a transient par2 probe error). The pipeline marks the
// delivery Corrupt so the engine treats it as an integrity failure and
// re-downloads, rather than shipping a broken file with a soft warning.
var ErrPar2Unrepairable = errors.New("par2: verification failed and repair not possible")

// par2Lookup probes whether the par2 binary is on PATH. It's a package var so
// tests can simulate a missing binary without touching the real PATH.
var par2Lookup = func() bool {
	_, err := exec.LookPath("par2")
	return err == nil
}

// Par2Available checks if par2cmdline is installed.
func Par2Available() bool { return par2Lookup() }

// Par2Verify verifies files using a par2 file. Returns nil on success,
// ErrPar2NotInstalled when the binary is missing (parity present but unchecked —
// the caller must surface it, NOT treat it as verified), a *Par2RepairableError
// when repair is possible, or another error on failure.
func Par2Verify(par2File string) error {
	if !Par2Available() {
		return ErrPar2NotInstalled
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
			return fmt.Errorf("%w:\n%s", ErrPar2Unrepairable, outStr)
		}
		return fmt.Errorf("par2 verify: %w\n%s", err, outStr)
	}

	log.Printf("[usenet] par2: verification OK")
	return nil
}

// Par2Repair attempts to repair files using par2 parity data.
func Par2Repair(par2File string) error {
	if !Par2Available() {
		return ErrPar2NotInstalled
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
