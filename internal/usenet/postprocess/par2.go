package postprocess

import (
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"regexp"
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

// Par2UnrepairableError carries par2's verdict alongside the list of target
// files it could not reconstruct. The caller uses Damaged to invalidate the
// resume state of exactly those files, so a retry re-fetches the broken ones
// and keeps everything par2 confirmed intact.
type Par2UnrepairableError struct {
	Output  string
	Damaged []string
	// Err is the underlying exec failure, when there was one. Kept so the exit
	// code survives into the message the user eventually sees.
	Err error
}

func (e *Par2UnrepairableError) Error() string {
	var b strings.Builder
	b.WriteString(ErrPar2Unrepairable.Error())
	if len(e.Damaged) > 0 {
		fmt.Fprintf(&b, " (damaged: %s)", strings.Join(e.Damaged, ", "))
	}
	if e.Err != nil {
		fmt.Fprintf(&b, " [%v]", e.Err)
	}
	b.WriteString("\n")
	b.WriteString(e.Output)
	return b.String()
}

// Unwrap keeps errors.Is(err, ErrPar2Unrepairable) working for every existing
// caller that classifies on the sentinel.
func (e *Par2UnrepairableError) Unwrap() error { return ErrPar2Unrepairable }

// par2TargetRe matches par2's per-file verdict lines, e.g.
//
//	Target: "release.part03.rar" - damaged. Found 12 of 20 data blocks.
//	Target: "thumbnail.jpg" - missing.
//	Target: "release.part01.rar" - found.
var par2TargetRe = regexp.MustCompile(`Target: "([^"]+)" - (\w+)`)

// parseDamagedTargets returns the target files par2 reported as anything other
// than intact ("found"/"complete").
func parseDamagedTargets(out string) []string {
	var damaged []string
	seen := make(map[string]bool)
	for _, m := range par2TargetRe.FindAllStringSubmatch(out, -1) {
		name, verdict := m[1], strings.ToLower(m[2])
		if verdict == "found" || verdict == "complete" || seen[name] {
			continue
		}
		seen[name] = true
		damaged = append(damaged, name)
	}
	return damaged
}

// trimPar2Output strips par2's carriage-return progress spam ("Loading: 3.8%\r
// Loading: 89.2%\r...", "Scanning: 0.5%\r..."), which on a multi-GB release
// runs to tens of kilobytes and used to push the actual verdict past the
// server's 2000-char error_message cap — leaving an undiagnosable failure in
// the UI. Only the last line of each \r-group survives, and the tail is capped.
func trimPar2Output(out string) string {
	var kept []string
	for _, line := range strings.Split(out, "\n") {
		if i := strings.LastIndex(line, "\r"); i >= 0 {
			line = line[i+1:]
		}
		line = strings.TrimSpace(line)
		if line == "" || strings.HasSuffix(line, "%") {
			continue
		}
		kept = append(kept, line)
	}
	joined := strings.Join(kept, "\n")
	const maxOut = 1200
	if len(joined) > maxOut {
		// The verdict ("Repair is not possible. You need N more recovery
		// blocks.") is the LAST thing par2 prints, so keep the tail.
		joined = "...\n" + joined[len(joined)-maxOut:]
	}
	return joined
}

// par2Lookup probes whether the par2 binary is on PATH. It's a package var so
// tests can simulate a missing binary without touching the real PATH.
var par2Lookup = func() bool {
	_, err := exec.LookPath("par2")
	return err == nil
}

// Par2Available checks if par2cmdline is installed.
func Par2Available() bool { return par2Lookup() }

// par2Command builds a par2 invocation forced to the C locale. The result
// classification below matches English stdout literals ("Repair is possible" /
// "Repair is not possible"); a localized par2 (non-C LANG/LC_ALL) would emit
// translated strings, so NONE would match and every verdict would collapse to
// the generic-error branch (deliver UNVERIFIED). LC_ALL=C pins the wording.
func par2Command(args ...string) *exec.Cmd {
	cmd := exec.Command("par2", args...)
	cmd.Env = append(os.Environ(), "LC_ALL=C", "LANG=C")
	return cmd
}

// Par2Verify verifies files using a par2 file. Returns nil on success,
// ErrPar2NotInstalled when the binary is missing (parity present but unchecked —
// the caller must surface it, NOT treat it as verified), a *Par2RepairableError
// when repair is possible, or another error on failure.
func Par2Verify(par2File string) error {
	if !Par2Available() {
		return ErrPar2NotInstalled
	}

	cmd := par2Command("verify", par2File)
	output, err := cmd.CombinedOutput()
	if err != nil {
		outStr := string(output)
		// Check if repair is possible
		if strings.Contains(outStr, "Repair is possible") {
			return &Par2RepairableError{Par2File: par2File, Damaged: parseDamagedTargets(outStr)}
		}
		if strings.Contains(outStr, "Repair is not possible") {
			return &Par2UnrepairableError{
				Output:  trimPar2Output(outStr),
				Damaged: parseDamagedTargets(outStr),
			}
		}
		return fmt.Errorf("par2 verify: %w\n%s", err, trimPar2Output(outStr))
	}

	log.Printf("[usenet] par2: verification OK")
	return nil
}

// Par2Repair attempts to repair files using par2 parity data.
func Par2Repair(par2File string) error {
	if !Par2Available() {
		return ErrPar2NotInstalled
	}

	cmd := par2Command("repair", par2File)
	output, err := cmd.CombinedOutput()
	if err != nil {
		outStr := string(output)
		// Only par2's own "can't do it" verdict means the DATA is beyond
		// recovery. Every other non-zero exit is the binary failing — OOM-killed
		// on a 40 GB release, a permission error, a bad build — and classifying
		// those as confirmed corruption made the engine throw away and re-fetch
		// a perfectly good download three times over a machine problem. Keep the
		// underlying error either way: the exit code is the whole diagnosis.
		if strings.Contains(outStr, "Repair is not possible") {
			return &Par2UnrepairableError{
				Output:  trimPar2Output(outStr),
				Damaged: parseDamagedTargets(outStr),
				Err:     err,
			}
		}
		return fmt.Errorf("par2 repair: %w\n%s", err, trimPar2Output(outStr))
	}

	log.Printf("[usenet] par2: repair successful")
	return nil
}

// Par2RepairableError indicates verification failed but repair is possible.
type Par2RepairableError struct {
	Par2File string
	Damaged  []string
}

func (e *Par2RepairableError) Error() string {
	return fmt.Sprintf("par2: verification failed, repair possible: %s", e.Par2File)
}
