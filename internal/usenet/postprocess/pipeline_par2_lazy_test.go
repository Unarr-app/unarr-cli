package postprocess

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// swapPar2Fns replaces the par2 verify/repair indirections for one test and
// restores them on cleanup.
func swapPar2Fns(t *testing.T, verify func(string) error, repair func(string) error) {
	t.Helper()
	origV, origR := par2VerifyFn, par2RepairFn
	if verify != nil {
		par2VerifyFn = verify
	}
	if repair != nil {
		par2RepairFn = repair
	}
	t.Cleanup(func() { par2VerifyFn, par2RepairFn = origV, origR })
}

func writeTestDelivery(t *testing.T) (dir string, files map[string]string) {
	t.Helper()
	dir = t.TempDir()
	par2Path := filepath.Join(dir, "release.par2")
	vid := filepath.Join(dir, "movie.mkv")
	if err := os.WriteFile(par2Path, []byte("fake parity index"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(vid, []byte("video data"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir, map[string]string{"release.par2": par2Path, "movie.mkv": vid}
}

// TestProcess_FetchParityRecovers verifies the lazy-volume flow: the index-only
// verify reports unrepairable damage ("need more blocks"), FetchParity runs
// ONCE, and the re-verify passes — the delivery is clean, not Corrupt.
func TestProcess_FetchParityRecovers(t *testing.T) {
	dir, files := writeTestDelivery(t)

	verifyCalls := 0
	swapPar2Fns(t, func(string) error {
		verifyCalls++
		if verifyCalls == 1 {
			return fmt.Errorf("%w: need 3 more recovery blocks", ErrPar2Unrepairable)
		}
		return nil
	}, func(string) error {
		t.Error("repair must not run when the re-verify passes")
		return nil
	})

	fetchCalls := 0
	res, err := Process(dir, files, Options{FetchParity: func() (map[string]string, error) {
		fetchCalls++
		volPath := filepath.Join(dir, "release.vol000+05.par2")
		if werr := os.WriteFile(volPath, []byte("recovery blocks"), 0o644); werr != nil {
			return nil, werr
		}
		return map[string]string{"release.vol000+05.par2": volPath}, nil
	}})
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if fetchCalls != 1 {
		t.Errorf("FetchParity calls = %d, want 1", fetchCalls)
	}
	if verifyCalls != 2 {
		t.Errorf("verify calls = %d, want 2 (initial + after fetch)", verifyCalls)
	}
	if res.Corrupt {
		t.Error("Corrupt must be false after a passing re-verify")
	}
	if res.VerifyNote != "" {
		t.Errorf("VerifyNote = %q, want empty", res.VerifyNote)
	}
	// Fetched volumes must land in downloadedFiles so cleanup removes them.
	if _, ok := files["release.vol000+05.par2"]; !ok {
		t.Error("fetched volume not merged into downloadedFiles")
	}
}

// TestProcess_FetchParityThenRepair covers damage that persists after the
// volumes arrive: re-verify still says repairable → repair runs → Repaired.
func TestProcess_FetchParityThenRepair(t *testing.T) {
	dir, files := writeTestDelivery(t)

	// Real par2 sequence: index-only verify says "repair not possible" (need
	// more blocks) → fetch volumes → re-verify now says "repair possible" →
	// repair. Only an Unrepairable verdict triggers the fetch (§4.3).
	verifyCalls := 0
	repairCalls := 0
	swapPar2Fns(t, func(p string) error {
		verifyCalls++
		if verifyCalls == 1 {
			return fmt.Errorf("%w: need 3 more recovery blocks", ErrPar2Unrepairable)
		}
		return &Par2RepairableError{Par2File: p}
	}, func(string) error {
		repairCalls++
		return nil
	})

	fetchCalls := 0
	res, err := Process(dir, files, Options{FetchParity: func() (map[string]string, error) {
		fetchCalls++
		volPath := filepath.Join(dir, "release.vol000+05.par2")
		if werr := os.WriteFile(volPath, []byte("recovery blocks"), 0o644); werr != nil {
			return nil, werr
		}
		return map[string]string{"release.vol000+05.par2": volPath}, nil
	}})
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if verifyCalls != 2 || repairCalls != 1 || fetchCalls != 1 {
		t.Errorf("verify/repair/fetch calls = %d/%d/%d, want 2/1/1", verifyCalls, repairCalls, fetchCalls)
	}
	if !res.Repaired {
		t.Error("Repaired must be true after a successful repair")
	}
	if res.Corrupt {
		t.Error("Corrupt must be false after a successful repair")
	}
}

// TestProcess_FetchParityFails: damage confirmed by the index but the recovery
// volumes can't be downloaded → the delivery is definitively Corrupt (the
// engine re-downloads), NOT a soft "unverified".
func TestProcess_FetchParityFails(t *testing.T) {
	dir, files := writeTestDelivery(t)

	swapPar2Fns(t, func(string) error {
		return fmt.Errorf("%w: need 3 more recovery blocks", ErrPar2Unrepairable)
	}, func(string) error {
		t.Error("repair must not run when parity fetch failed")
		return nil
	})

	res, err := Process(dir, files, Options{FetchParity: func() (map[string]string, error) {
		return nil, errors.New("all 5 par2 downloads failed")
	}})
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if !res.Corrupt {
		t.Error("Corrupt must be true when damage is confirmed and parity is unavailable")
	}
	if !strings.Contains(res.VerifyNote, "recovery volumes") {
		t.Errorf("VerifyNote = %q, want mention of recovery volumes", res.VerifyNote)
	}
}

// TestProcess_TransientReverifyKeepsCorrupt: damage confirmed by verify #1,
// FetchParity succeeds, but the post-fetch re-verify fails TRANSIENTLY (binary
// crash / truncated volume / I/O) — the confirmed-corrupt delivery must stay
// Corrupt, not be downgraded to a soft "unverified" and shipped (§3.1).
func TestProcess_TransientReverifyKeepsCorrupt(t *testing.T) {
	dir, files := writeTestDelivery(t)

	verifyCalls := 0
	swapPar2Fns(t, func(string) error {
		verifyCalls++
		if verifyCalls == 1 {
			return fmt.Errorf("%w: need 3 more recovery blocks", ErrPar2Unrepairable)
		}
		// Post-fetch re-verify dies transiently (no typed par2 verdict).
		return fmt.Errorf("par2 killed by signal: killed")
	}, func(string) error {
		t.Error("repair must not run when the re-verify errored transiently")
		return nil
	})

	res, err := Process(dir, files, Options{FetchParity: func() (map[string]string, error) {
		volPath := filepath.Join(dir, "release.vol000+05.par2")
		if werr := os.WriteFile(volPath, []byte("recovery blocks"), 0o644); werr != nil {
			return nil, werr
		}
		return map[string]string{"release.vol000+05.par2": volPath}, nil
	}})
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if !res.Corrupt {
		t.Error("Corrupt must stay true: verify #1 proved damage, a transient re-verify must not downgrade it")
	}
	if !strings.Contains(res.VerifyNote, "treated as corrupt") {
		t.Errorf("VerifyNote = %q, want it to flag the confirmed-damage-through-transient case", res.VerifyNote)
	}
}

// TestProcess_RepairableSkipsFetch: a Repairable verdict already has enough
// local parity — it must NOT trigger the volume fetch (§4.3), just repair.
func TestProcess_RepairableSkipsFetch(t *testing.T) {
	dir, files := writeTestDelivery(t)

	swapPar2Fns(t, func(p string) error { return &Par2RepairableError{Par2File: p} },
		func(string) error { return nil })

	res, err := Process(dir, files, Options{FetchParity: func() (map[string]string, error) {
		t.Error("FetchParity must not run for a Repairable verdict (local parity suffices)")
		return nil, nil
	}})
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if !res.Repaired || res.Corrupt {
		t.Errorf("Repairable path: got Repaired=%v Corrupt=%v, want true/false", res.Repaired, res.Corrupt)
	}
}

// TestProcess_NoPar2CleanDelivery: a delivery that ships NO parity verifies as
// clean (no note, not corrupt) — the par2 step is a no-op (§4.6).
func TestProcess_NoPar2CleanDelivery(t *testing.T) {
	dir := t.TempDir()
	vid := filepath.Join(dir, "movie.mkv")
	if err := os.WriteFile(vid, []byte("video data"), 0o644); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{"movie.mkv": vid}

	swapPar2Fns(t, func(string) error {
		t.Error("par2 verify must not run when no parity ships")
		return nil
	}, nil)

	res, err := Process(dir, files, Options{})
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if res.Corrupt || res.VerifyNote != "" {
		t.Errorf("no-parity delivery: got Corrupt=%v note=%q, want clean", res.Corrupt, res.VerifyNote)
	}
	if res.FinalPath != vid {
		t.Errorf("FinalPath = %q, want %q", res.FinalPath, vid)
	}
}

// TestProcess_NoFetchWhenVerifyOK: the happy path never touches FetchParity.
func TestProcess_NoFetchWhenVerifyOK(t *testing.T) {
	dir, files := writeTestDelivery(t)

	swapPar2Fns(t, func(string) error { return nil }, nil)

	res, err := Process(dir, files, Options{FetchParity: func() (map[string]string, error) {
		t.Error("FetchParity must not run when the index verify passes")
		return nil, nil
	}})
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if res.Corrupt || res.Repaired || res.VerifyNote != "" {
		t.Errorf("clean verify: got Corrupt=%v Repaired=%v note=%q", res.Corrupt, res.Repaired, res.VerifyNote)
	}
}

// TestProcess_UnrepairableWithoutFetcher: no FetchParity (all parity already
// local) and verify says unrepairable → Corrupt, same as before lazy volumes.
func TestProcess_UnrepairableWithoutFetcher(t *testing.T) {
	dir, files := writeTestDelivery(t)

	swapPar2Fns(t, func(string) error {
		return fmt.Errorf("%w: need 3 more recovery blocks", ErrPar2Unrepairable)
	}, nil)

	res, err := Process(dir, files, Options{})
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if !res.Corrupt {
		t.Error("Corrupt must be true for unrepairable damage with no parity fetcher")
	}
}

// TestProcess_RepairRenamePickedUp: after a repair that renamed an obfuscated
// file (stale map path), the renamed file is collected from the dir scan and
// par2's "<name>.1" damaged-original leftovers are skipped.
func TestProcess_RepairRenamePickedUp(t *testing.T) {
	dir := t.TempDir()
	par2Path := filepath.Join(dir, "release.par2")
	obfuscated := filepath.Join(dir, "a1b2c3d4e5f60718.mkv")
	if err := os.WriteFile(par2Path, []byte("fake parity"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(obfuscated, []byte("video data"), 0o644); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{"release.par2": par2Path, "a1b2c3d4e5f60718.mkv": obfuscated}

	renamed := filepath.Join(dir, "Movie.2024.1080p.mkv")
	swapPar2Fns(t, func(p string) error {
		return &Par2RepairableError{Par2File: p}
	}, func(string) error {
		// Simulate par2 repair renaming the misnamed file and leaving the
		// damaged original aside as "<name>.1".
		if err := os.Rename(obfuscated, renamed); err != nil {
			return err
		}
		return os.WriteFile(obfuscated+".1", []byte("damaged original"), 0o644)
	})

	res, err := Process(dir, files, Options{})
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if !res.Repaired {
		t.Fatal("Repaired must be true")
	}
	if res.FinalPath != renamed {
		t.Errorf("FinalPath = %q, want renamed %q", res.FinalPath, renamed)
	}
	for _, f := range res.Files {
		if f == obfuscated {
			t.Errorf("stale pre-rename path %q must not be in Files", f)
		}
		if strings.HasSuffix(f, ".1") {
			t.Errorf("par2 damaged-original leftover %q must not be in Files", f)
		}
	}
}
