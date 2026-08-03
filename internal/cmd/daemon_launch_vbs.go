package cmd

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"strings"
	"unicode/utf16"

	"github.com/Unarr-app/unarr-cli/internal/agent"
)

const (
	// maxRelaunch is how many CONSECUTIVE fast failures the shim tolerates before
	// giving up and letting the task report a failed Last Result. A daemon that
	// cannot start (bad config, missing credential) must not spin forever; one
	// that merely died should always come back.
	maxRelaunch = 5
	// healthyRunSecs is how long a run must last to count as healthy and clear the
	// failure budget. Ten minutes is far longer than any startup crash loop and
	// far shorter than a normal session.
	healthyRunSecs = 600
)

// launcherVBSName is the shim the scheduled task runs instead of invoking the
// daemon directly. It lives next to the log in the data dir.
const launcherVBSName = "unarr-launch.vbs"

// buildLauncherVBS renders a VBScript shim that starts the daemon with NO
// console window, ever.
//
// Why a VBS shim at all — the boot flash the previous form could not kill:
// unarr.exe is a CONSOLE-subsystem binary (the CLI needs stdout/stderr attached
// to a terminal, so it can't be built -H=windowsgui like the tray). When the
// scheduled task launched it — even wrapped in `powershell -WindowStyle Hidden`
// — Task Scheduler allocated a console at logon and only hid it afterwards: the
// window is created, drawn, THEN hidden, which is the visible flash the user
// saw before the desktop app came up. winproc.HideWindow (1.7.6) fixes the
// windows unarr *itself* spawns, but it can't touch the ROOT process the task
// launches — that's the Task Scheduler's job.
//
// wscript.exe is a GUI-subsystem host: it never allocates a console. Its
// WshShell.Run(cmd, 0, …) launches the child with window style 0 (hidden) and —
// crucially — does NOT create a console window for the child even when the child
// is console-subsystem. So the daemon starts fully headless from first
// instruction: no flash, on Windows 10 and 11 alike (wscript predates both;
// unlike `conhost --headless`, which is Win11-only).
//
// Logging is preserved by wrapping the daemon in `cmd /c "… >> log 2>&1"`, also
// run hidden (window 0). Both launches redirect to the log; if the redirected
// launch can't run (log locked by AV / a stale handle → cmd exits non-zero) the
// shim retries once, still redirected, so a failed start is never silent. The
// daemon is not gated on logging succeeding.
//
// TWO LOG FILES, TWO OWNERS — and they MUST be different filenames:
//
//   - unarr.log belongs to the daemon (`start --log-file …`). It opens the file
//     O_APPEND, which Go maps to a real FILE_APPEND_DATA handle, and rotates it
//     by renaming it aside. That is the only rotation that shrinks a live file
//     on Windows.
//   - unarr.boot.log belongs to cmd.exe, via the `>>` redirect. It collects what
//     bypasses log.SetOutput and would otherwise be lost: the start banner,
//     cobra's fatal error print for a start that never got going, a Go panic
//     dump.
//
// Pointing both at unarr.log would be strictly worse than the bug this fixes:
// cmd.exe's redirect grants only FILE_SHARE_READ, so the daemon's own
// FILE_APPEND_DATA open would be refused and it would end up with NO log at all.
// The same sharing rule is why the boot log cannot be copy-truncated while cmd
// holds it (os.Truncate is OpenFile(O_WRONLY)+Ftruncate — a sharing violation,
// measured on the VM harness), and therefore why the shim bounds it ITSELF, by
// rename, at the top of the loop where nothing holds the file.
//
// SUPERVISION: the shim restarts the daemon itself. It has to — the scheduled
// task will not.
//
// The task carries <RestartOnFailure Count=3 Interval=PT1M>, which reads like a
// supervisor and is not one: measured on real Windows (Win11 26200, logon
// trigger, task XML confirmed), killing the daemon leaves the task at
// `Status: Ready, Last Result: 1` and NOTHING restarts it — not after one
// minute, not after five. RestartOnFailure covers the task failing to *start*,
// not its action exiting non-zero. So a daemon that died seconds after logon
// stayed dead until the next logon; that is the field report this fixes.
//
// The shim is the only component positioned to fix it: the task keeps wscript
// alive for the whole session, so a loop here IS the supervisor. It relaunches
// with a backoff, gives up after maxRelaunch consecutive fast failures (so a
// genuinely broken install cannot spin forever), and resets that budget after a
// run long enough to count as healthy — the same shape as systemd's start-limit
// logic.
//
// It still ends in an explicit WScript.Quit: the exit code is what `schtasks
// /query` reports as Last Result, so an operator (and the task history) can see
// whether the agent gave up or was stopped on purpose.
//
// STOP vs DEATH: the daemon's own exit status cannot tell them apart —
// `unarr stop` is taskkill /f, so a user-initiated stop exits exactly like an AV
// kill, while the auto-upgrade exits 0 and DOES want a relaunch. The verdict
// comes from the stop-intent marker (agent.StopIntentFileName) instead: present
// → the stop was asked for → quit 0 and stay down; absent → relaunch.
// Defaulting to "relaunch" is deliberate: an unnecessary restart is a far
// smaller failure than an agent that is silently gone until the next logon.
//
// ENCODING: the returned script MUST be written to disk as UTF-16LE+BOM (see
// buildLauncherVBSBytes). Windows Script Host reads a BOM-less .vbs through the
// system ANSI code page, NOT UTF-8 — so a UTF-8 file whose paths contain a
// non-ASCII username (Zoë, André, 伊藤 …) would be mojibake'd and the daemon
// would silently never start at logon. This is the same lesson the task XML
// already encodes (schtasks likewise needs UTF-16). We build the string here
// and let buildLauncherVBSBytes do the UTF-16LE+BOM encoding, mirroring
// buildWindowsTaskXML / buildWindowsTaskXMLBytes.
//
// bootMaxBytes is the boot log's rotation budget, or 0 for "emit no trim at
// all" — rotation is opt-in (see bootLogTrimBytes), and with it off the
// generated script must leave the boot log's ring alone like every other
// rotation path. Passed in rather than read from config here so this stays a
// pure renderer.
func buildLauncherVBS(binPath, logDir string, bootMaxBytes int64) string {
	dir := strings.TrimRight(logDir, `\`)
	logPath := dir + `\` + logFileName
	bootPath := dir + `\` + bootLogFileName

	// The command cmd.exe runs. Each path is wrapped in real double-quotes, which
	// is BOTH necessary and sufficient: quotes make spaces safe AND neutralise
	// cmd's metacharacters (& | < > ( )) — a Windows username can legally contain
	// them ("Tom & Jerry", "R&D"). Do NOT caret-escape inside the quotes: cmd
	// does not process `^` as an escape between double-quotes, so `Tom ^& Jerry`
	// would make it look for a file whose name literally contains a caret. The
	// nested-quote idiom `cmd /c ""prog" args"` is standard: cmd /c strips the
	// single outer pair, leaving each path individually quoted and protected.
	//
	// --log-file rides on BOTH forms: a launch that could not be redirected must
	// still give the daemon its own log, or that run would be entirely silent.
	cmdFor := func(withRedirect bool) string {
		start := `""` + binPath + `" start --log-file "` + logPath + `"`
		if withRedirect {
			return `cmd /c ` + start + ` >> "` + bootPath + `" 2>&1"`
		}
		return `cmd /c ` + start + `"`
	}

	// Marker the shim consults to tell a requested stop from a death. Built from
	// logDir (the data dir the installer just wrote the shim into) rather than
	// resolved at run time — by the time this code runs the daemon is gone.
	stopMarker := strings.TrimRight(logDir, `\`) + `\` + agent.StopIntentFileName

	// Run(cmd, 0, True): window style 0 = hidden, bWaitOnReturn = True so the shim
	// blocks for the life of the daemon. That is what keeps wscript resident (and
	// the task in the Running state) — and what lets the loop below notice the
	// moment the daemon goes away.
	var b strings.Builder
	b.WriteString("' unarr daemon launcher — generated by `unarr daemon install`.\n")
	b.WriteString("' Supervises the daemon: Task Scheduler's RestartOnFailure does NOT\n")
	b.WriteString("' act on a non-zero action exit code (measured). See daemon_launch_vbs.go.\n")
	// On Error Resume Next FIRST: a failed CreateObject must not abort the script
	// before the Quit at the bottom, or the task would again see a bare success.
	b.WriteString("On Error Resume Next\n")
	b.WriteString("Set sh = CreateObject(\"WScript.Shell\")\n")
	b.WriteString("Set fso = CreateObject(\"Scripting.FileSystemObject\")\n")
	b.WriteString(fmt.Sprintf("maxTries = %d\n", maxRelaunch))
	b.WriteString(fmt.Sprintf("healthySecs = %d\n", healthyRunSecs))
	b.WriteString("tries = 0\n")
	b.WriteString("Do\n")
	writeBootLogTrim(&b, bootPath, bootMaxBytes)
	b.WriteString("  started = Timer\n")
	b.WriteString("  rc = sh.Run(" + vbsQuote(cmdFor(true)) + ", 0, True)\n")
	// A launch that could not run at all (Err set) still counts as an attempt —
	// falling through to the backoff below rather than retrying instantly.
	b.WriteString("  If Err.Number <> 0 Then\n")
	b.WriteString("    Err.Clear\n")
	b.WriteString("    rc = -1\n")
	b.WriteString("  End If\n")
	b.WriteString("  ran = Timer - started\n")
	// Timer counts seconds since midnight, so a run spanning midnight goes
	// negative. Without this a daemon that survived the night would look like an
	// instant crash and burn an attempt.
	b.WriteString("  If ran < 0 Then ran = ran + 86400\n")
	// Was this stop asked for? fso is Nothing only if CreateObject failed; treat
	// that as "not stopped on purpose" so the agent still comes back — the safe
	// direction (see the doc comment).
	b.WriteString("  Err.Clear\n")
	b.WriteString("  stopped = False\n")
	b.WriteString("  If Not fso Is Nothing Then\n")
	b.WriteString("    stopped = fso.FileExists(" + vbsQuote(stopMarker) + ")\n")
	b.WriteString("  End If\n")
	b.WriteString("  If Err.Number <> 0 Then stopped = False\n")
	b.WriteString("  If stopped Then WScript.Quit 0\n")
	// A run long enough to count as healthy clears the failure budget, so an
	// agent that has been up for hours and then dies is not judged by crashes
	// from last week. Mirrors systemd's start-limit window.
	b.WriteString("  If ran >= healthySecs Then tries = 0\n")
	b.WriteString("  tries = tries + 1\n")
	b.WriteString("  If tries > maxTries Then WScript.Quit 1\n")
	// Escalating backoff, capped: 15s, 30s, 60s, 120s, 120s… Fast enough that a
	// transient death is invisible to the user, slow enough that a broken install
	// is not a spin loop.
	b.WriteString("  waitMs = 15000 * (2 ^ (tries - 1))\n")
	b.WriteString("  If waitMs > 120000 Then waitMs = 120000\n")
	b.WriteString("  WScript.Sleep waitMs\n")
	// Re-check before relaunching: the user may have asked for a stop DURING the
	// backoff, and relaunching then would undo it.
	b.WriteString("  Err.Clear\n")
	b.WriteString("  If Not fso Is Nothing Then\n")
	b.WriteString("    If fso.FileExists(" + vbsQuote(stopMarker) + ") Then WScript.Quit 0\n")
	b.WriteString("  End If\n")
	b.WriteString("Loop\n")
	return b.String()
}

// writeBootLogTrim emits the boot log's rotation, which lives HERE because it
// can live nowhere else: cmd.exe holds that file for the whole life of a run and
// grants only FILE_SHARE_READ, so no process — not even the daemon — can
// truncate or rename it while a daemon is up. The top of the relaunch loop is
// the one moment nothing holds it.
//
// SIZE-CHECKED, never unconditional: rotating on every launch would push the
// first (and most interesting) crash out of the ring after two relaunches, which
// is precisely the evidence a crash loop is being kept for. One rotated slot,
// renamed rather than copied, at maxBytes.
//
// maxBytes <= 0 emits NOTHING. Rotation is opt-in and off by default, and this
// is the one rotator that cannot re-read the config at run time, so "off" has to
// mean "the script was generated without a trim in it". A shim that kept its own
// baked 2 MB threshold would be the single ring still mutating on a default
// install — the exact thing the descope removed everywhere else.
//
// Every step is best-effort under the script's `On Error Resume Next`, and Err
// is cleared afterwards so a failed trim cannot be mistaken for a failed launch
// by the check that follows sh.Run.
//
// THE ORDER IS THE SAME INVARIANT logging.rotateThroughStaging enforces in Go,
// spelled out by hand because VBScript cannot import it: move the LIVE file to
// a staging name FIRST, and only once that worked delete the old slot and put
// the staging file in it. Deleting .1 first — which is what this did — threw
// away the crash evidence the one-slot ring exists for whenever the move then
// failed, and the move fails for the ordinary reasons: an antivirus, Windows
// Search, or a support session with `Get-Content -Wait` on the boot log.
//
// "Did the move work?" is tested as "is the live file gone?", which is exact
// under On Error Resume Next and, unlike testing for the staging file, cannot
// be fooled by a stale staging file left by a run that died mid-trim.
func writeBootLogTrim(b *strings.Builder, bootPath string, maxBytes int64) {
	if maxBytes <= 0 {
		return
	}
	staging := bootPath + ".rotating"
	b.WriteString("  Err.Clear\n")
	b.WriteString("  If Not fso Is Nothing Then\n")
	b.WriteString("    If fso.FileExists(" + vbsQuote(bootPath) + ") Then\n")
	b.WriteString(fmt.Sprintf("      If fso.GetFile(%s).Size > %d Then\n",
		vbsQuote(bootPath), maxBytes))
	b.WriteString("        If fso.FileExists(" + vbsQuote(staging) + ") Then fso.DeleteFile " + vbsQuote(staging) + ", True\n")
	b.WriteString("        fso.MoveFile " + vbsQuote(bootPath) + ", " + vbsQuote(staging) + "\n")
	b.WriteString("        If Not fso.FileExists(" + vbsQuote(bootPath) + ") Then\n")
	b.WriteString("          If fso.FileExists(" + vbsQuote(bootPath+".1") + ") Then fso.DeleteFile " + vbsQuote(bootPath+".1") + ", True\n")
	b.WriteString("          fso.MoveFile " + vbsQuote(staging) + ", " + vbsQuote(bootPath+".1") + "\n")
	b.WriteString("        End If\n")
	b.WriteString("      End If\n")
	b.WriteString("    End If\n")
	b.WriteString("  End If\n")
	b.WriteString("  Err.Clear\n")
}

// buildLauncherVBSBytes encodes the shim as UTF-16LE with a BOM — the encoding
// Windows Script Host needs to read non-ASCII paths correctly (a BOM-less file
// is decoded via the ANSI code page). Mirrors buildWindowsTaskXMLBytes.
func buildLauncherVBSBytes(binPath, logDir string, bootMaxBytes int64) []byte {
	s := buildLauncherVBS(binPath, logDir, bootMaxBytes)
	u16 := utf16.Encode([]rune(s))
	buf := &bytes.Buffer{}
	buf.Write([]byte{0xFF, 0xFE}) // UTF-16LE BOM
	for _, r := range u16 {
		_ = binary.Write(buf, binary.LittleEndian, r)
	}
	return buf.Bytes()
}

// vbsQuote turns a Go string into a VBScript double-quoted literal, doubling any
// inner double-quote — the only escaping VBScript string literals need.
func vbsQuote(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}
