package cmd

import (
	"strconv"
	"strings"
	"testing"

	"github.com/Unarr-app/unarr-cli/internal/agent"
)

const (
	vbsTestBin = `C:\Program Files\unarr\unarr.exe`
	vbsTestDir = `C:\Users\tester\AppData\Local\unarr`
)

// THE most important invariant in the log-ownership design, and the one a
// plausible-looking edit breaks silently.
//
// The daemon opens its log O_APPEND, which Go maps to FILE_APPEND_DATA — a
// write right. cmd.exe's `>>` redirect grants only FILE_SHARE_READ. So if the
// shim ever redirects into the SAME file it hands to --log-file, the daemon's
// own open is refused and it ends up with NO log at all: strictly worse than
// the copy-truncate bug this design replaces. The two paths must differ.
func TestLauncherVBSRedirectsSomewhereOtherThanTheOwnedLog(t *testing.T) {
	script := buildLauncherVBS(vbsTestBin, vbsTestDir, bootLogMaxBytes)

	ownedLog := vbsTestDir + `\` + logFileName
	bootLog := vbsTestDir + `\` + bootLogFileName

	if !strings.Contains(script, inVBSLiteral(`start --log-file "`+ownedLog+`"`)) {
		t.Errorf("the shim does not hand the daemon its own log (--log-file %s):\n%s", ownedLog, script)
	}
	if !strings.Contains(script, inVBSLiteral(`>> "`+bootLog+`" 2>&1`)) {
		t.Errorf("the shim does not redirect into %s:\n%s", bootLog, script)
	}
	if strings.Contains(script, inVBSLiteral(`>> "`+ownedLog+`"`)) {
		t.Fatalf("the shim redirects cmd.exe into the SAME file the daemon owns (%s); "+
			"cmd grants only FILE_SHARE_READ, so the daemon's FILE_APPEND_DATA open would be "+
			"refused and it would have no log at all:\n%s", ownedLog, script)
	}
}

// inVBSLiteral renders a fragment of the cmd.exe command line as it appears
// once embedded in a VBScript string literal, where every double quote is
// doubled. Asserting against the raw fragment would never match.
func inVBSLiteral(s string) string { return strings.ReplaceAll(s, `"`, `""`) }

// Nothing in Go can bound a file cmd.exe holds — os.Truncate is
// OpenFile(O_WRONLY)+Ftruncate, and that open is a sharing violation against the
// redirect (measured on the VM harness). The only moment the boot log is
// unheld is between two runs, inside this loop, so the trim has to live here.
func TestLauncherVBSTrimsTheBootLogItself(t *testing.T) {
	script := buildLauncherVBS(vbsTestBin, vbsTestDir, bootLogMaxBytes)
	bootLog := vbsTestDir + `\` + bootLogFileName

	for _, want := range []string{
		`fso.GetFile("` + bootLog + `").Size > `,
		`fso.MoveFile "` + bootLog + `", "` + bootLog + `.rotating"`,
		`fso.MoveFile "` + bootLog + `.rotating", "` + bootLog + `.1"`,
	} {
		if !strings.Contains(script, want) {
			t.Errorf("the shim never trims the boot log — missing %q:\n%s", want, script)
		}
	}

	// SAME INVARIANT AS THE GO ROTATOR: the live file moves aside FIRST, and the
	// one history slot is only deleted once that worked. Deleting .1 up front
	// destroys the crash evidence this ring exists for every time the move is
	// blocked (antivirus, Windows Search, a `Get-Content -Wait` left open) — and
	// nothing is produced in exchange.
	moveLive := strings.Index(script, `fso.MoveFile "`+bootLog+`", "`+bootLog+`.rotating"`)
	dropSlot := strings.Index(script, `fso.DeleteFile "`+bootLog+`.1"`)
	if moveLive < 0 || dropSlot < 0 || dropSlot < moveLive {
		t.Errorf("the trim drops the history slot (%d) before moving the live file aside (%d):\n%s",
			dropSlot, moveLive, script)
	}
	// And it only gets there when the move actually worked.
	if !strings.Contains(script, `If Not fso.FileExists("`+bootLog+`") Then`) {
		t.Errorf("the trim never checks that the live file really moved:\n%s", script)
	}

	// SIZE-CHECKED, not unconditional. Rotating on every launch would push the
	// first and most interesting crash out of the one-slot ring after two
	// relaunches, which is exactly the evidence a crash loop is kept for.
	if !strings.Contains(script, "> "+strconv.Itoa(bootLogMaxBytes)+" Then") {
		t.Errorf("the boot-log trim is not gated on bootLogMaxBytes (%d):\n%s", bootLogMaxBytes, script)
	}

	// It must run BEFORE the launch, in the window where nothing holds the file.
	trim := strings.Index(script, "fso.MoveFile")
	run := strings.Index(script, "rc = sh.Run(")
	if trim < 0 || run < 0 || trim > run {
		t.Errorf("the boot-log trim (%d) does not precede the launch (%d): cmd.exe holds the file from sh.Run onwards", trim, run)
	}
}

// test/windows/README.md records the measurement: the scheduled task's
// RestartOnFailure does NOT act on a non-zero action exit code, so the relaunch
// loop in this shim IS the supervisor. A refactor that drops the loop, the
// stop-intent check or the give-up budget silently un-fixes "the agent is gone
// until the next logon".
func TestLauncherVBSKeepsSupervising(t *testing.T) {
	script := buildLauncherVBS(vbsTestBin, vbsTestDir, bootLogMaxBytes)

	for _, want := range []string{
		"Do\n",        // the relaunch loop itself
		"\nLoop\n",    // …which must actually loop
		"maxTries",    // give up on a genuinely broken install
		"healthySecs", // a long run clears the failure budget
		"WScript.Quit 0",
		"WScript.Quit 1",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("the supervision loop lost %q — RestartOnFailure will not cover for it:\n%s", want, script)
		}
	}
}

// TestLauncherVBSBackoffAnswersAStartRequest: the relaunch backoff polls for the
// start-now marker instead of sleeping blind. While the shim waits the task is
// Running, and `schtasks /run` against it starts nothing — so without the poll a
// Resume clicked after a crash did nothing for up to two minutes.
func TestLauncherVBSBackoffAnswersAStartRequest(t *testing.T) {
	script := buildLauncherVBS(vbsTestBin, vbsTestDir, bootLogMaxBytes)
	marker := vbsQuote(strings.TrimRight(vbsTestDir, `\`) + `\` + agent.StartRequestFileName)

	if strings.Contains(script, "WScript.Sleep waitMs") {
		t.Error("the backoff is one blind Sleep again: a start request cannot cut it short")
	}
	for _, want := range []string{
		"Do While waited < waitMs",
		"fso.FileExists(" + marker + ")",
		"fso.DeleteFile " + marker + ", True", // consumed, or it would skip every later backoff
		// A person asked: a fresh failure budget, then out of the wait. Matched as
		// a sequence — "tries = 0" alone also matches the loop's initialisation.
		"      tries = 0\n      Exit Do\n",
		"WScript.Sleep 1000",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("the backoff lost %q:\n%s", want, script)
		}
	}
	// The stop re-check must still follow the wait: a Pause during the backoff
	// has to win over the relaunch.
	wait := strings.Index(script, "Do While waited < waitMs")
	stop := strings.LastIndex(script, "If fso.FileExists("+vbsQuote(strings.TrimRight(vbsTestDir, `\`)+`\`+agent.StopIntentFileName)+") Then WScript.Quit 0")
	if wait < 0 || stop < wait {
		t.Error("the stop-intent check no longer follows the backoff wait")
	}
}
