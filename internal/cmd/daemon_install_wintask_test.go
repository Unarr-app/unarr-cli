package cmd

import (
	"encoding/xml"
	"runtime"
	"strings"
	"testing"
	"unicode/utf16"

	"github.com/Unarr-app/unarr-cli/internal/agent"
)

func TestBuildWindowsTaskXML(t *testing.T) {
	data := serviceData{BinPath: `C:\Program Files\unarr\unarr.exe`, User: `DESKTOP-ABC\alice`}
	got := buildWindowsTaskXML(data, `C:\Users\alice\AppData\Local\unarr`)

	// Must be parseable back into the task struct (well-formed XML, correct
	// escaping of the Windows paths and the embedded PowerShell command).
	var rt taskXML
	body := strings.SplitN(got, "\n", 2)[1] // drop the <?xml …?> declaration
	if err := xml.Unmarshal([]byte(body), &rt); err != nil {
		t.Fatalf("generated task XML does not parse: %v\n%s", err, got)
	}

	// The three reliability settings the flag form lacked must be present.
	if rt.Triggers.Logon.Delay == "" {
		t.Error("logon trigger has no <Delay> — login start-up race is unfixed")
	}
	if rt.Settings.RestartOnFailure == nil || rt.Settings.RestartOnFailure.Count < 1 {
		t.Error("no RestartOnFailure — a transient early exit leaves the agent dead")
	}
	if !rt.Settings.StartWhenAvailable {
		t.Error("StartWhenAvailable=false — a missed logon trigger never recovers")
	}

	// The action must launch the VBScript shim through wscript.exe — a
	// GUI-subsystem host that allocates no console. Anything that hosts a
	// console (the daemon directly, or a powershell wrapper) reintroduces the
	// logon flash, since unarr.exe is console-subsystem.
	if rt.Actions.Exec.Command != "wscript.exe" {
		t.Errorf("task action must launch wscript.exe (no console), got %q", rt.Actions.Exec.Command)
	}
	if !strings.Contains(rt.Actions.Exec.Arguments, launcherVBSName) {
		t.Errorf("action arguments missing the launcher shim %q: %q", launcherVBSName, rt.Actions.Exec.Arguments)
	}

	// Regression guard: the console flash came from launching a console host at
	// logon. The action must never invoke powershell or the daemon binary
	// directly again.
	if strings.Contains(rt.Actions.Exec.Command, "powershell") {
		t.Error("action launches powershell — a hidden console still flashes at logon")
	}
	if strings.Contains(rt.Actions.Exec.Arguments, data.BinPath) {
		t.Error("action launches the console-subsystem daemon directly — flashes a console at logon")
	}

	// The runtime user must be carried into both the principal and trigger.
	if rt.Principals.Principal.UserID != data.User {
		t.Errorf("principal UserId = %q, want %q", rt.Principals.Principal.UserID, data.User)
	}

	// The prolog must declare UTF-16 (schtasks reads it and rejects a mismatch).
	if !strings.HasPrefix(got, `<?xml version="1.0" encoding="UTF-16"?>`) {
		t.Errorf("prolog must declare UTF-16, got: %q", strings.SplitN(got, "\n", 2)[0])
	}
}

func TestBuildLauncherVBS(t *testing.T) {
	bin := `C:\Program Files\unarr\unarr.exe`
	logDir := `C:\Users\alice\AppData\Local\unarr`
	vbs := buildLauncherVBS(bin, logDir)

	// The daemon binary and its `start` verb now live in the shim, not the XML.
	if !strings.Contains(vbs, bin) {
		t.Errorf("VBS missing daemon bin path: %s", vbs)
	}
	if !strings.Contains(vbs, "start") {
		t.Errorf("VBS missing `start` verb: %s", vbs)
	}

	// Must run hidden: WshShell.Run with window style 0. A non-zero style would
	// show a window — the whole point is zero UI at logon.
	if !strings.Contains(vbs, ", 0, True)") {
		t.Errorf("VBS must Run with window style 0 (hidden): %s", vbs)
	}

	// Log capture + no-log fallback: the daemon must still come up if the log
	// path can't be opened (locked by AV / a stale handle). Both a redirected
	// and a plain launch must be present.
	if !strings.Contains(vbs, `\unarr.log`) {
		t.Errorf("VBS missing log redirect: %s", vbs)
	}
	if strings.Count(vbs, "sh.Run") < 2 {
		t.Errorf("VBS has no no-log fallback launch (want 2 Run calls): %s", vbs)
	}

	// VBScript escapes an inner double-quote by doubling it. The daemon path is
	// wrapped in quotes for cmd.exe, so those quotes must appear doubled inside
	// the VBScript string literal — a single quote would terminate the literal
	// and produce a syntax error at logon (silent: the task just never starts).
	if !strings.Contains(vbs, `""`+bin+`""`) {
		t.Errorf("daemon path quotes not doubled for VBScript literal: %s", vbs)
	}
}

// TestBuildLauncherVBSQuotesUsernamePath guards a data dir whose path carries a
// space (Windows usernames routinely do) — the redirect target must stay quoted
// so cmd.exe treats it as one argument.
func TestBuildLauncherVBSQuotesUsernamePath(t *testing.T) {
	vbs := buildLauncherVBS(`C:\unarr\unarr.exe`, `C:\Users\Ana Ruiz\AppData\Local\unarr`)
	if !strings.Contains(vbs, `"C:\Users\Ana Ruiz\AppData\Local\unarr\unarr.log"`) {
		t.Errorf("log path with a space is not quoted for cmd.exe: %s", vbs)
	}
}

// TestBuildLauncherVBSBytesUTF16 is the DEFECT-1 regression guard: the shim must
// hit disk as UTF-16LE+BOM. A BOM-less file is decoded by Windows Script Host
// through the ANSI code page, which mojibake's a non-ASCII username in the
// embedded paths → the daemon silently never starts at logon.
func TestBuildLauncherVBSBytesUTF16(t *testing.T) {
	// Non-ASCII username exercises the encoding path (corrupts under ANSI/UTF-8).
	b := buildLauncherVBSBytes(`C:\Users\Zoë\unarr\unarr.exe`, `C:\Users\Zoë\AppData\Local\unarr`)
	if len(b) < 2 || b[0] != 0xFF || b[1] != 0xFE {
		t.Fatalf("missing UTF-16LE BOM (FF FE); first bytes: % x", b[:min(4, len(b))])
	}
	// Decode UTF-16LE back and confirm the non-ASCII username round-trips.
	u16 := make([]uint16, 0, (len(b)-2)/2)
	for i := 2; i+1 < len(b); i += 2 {
		u16 = append(u16, uint16(b[i])|uint16(b[i+1])<<8)
	}
	text := string(utf16.Decode(u16))
	if !strings.Contains(text, `Zoë`) {
		t.Error("non-ASCII username did not round-trip through UTF-16LE")
	}
	if !strings.Contains(text, "WScript.Shell") {
		t.Error("decoded shim is not the launcher script")
	}
}

// TestBuildLauncherVBSMetacharUsername guards the cmd-metacharacter case: a
// Windows username can contain & ( ) etc. The path is wrapped in double-quotes,
// which already neutralises those for cmd.exe — so the path must appear VERBATIM
// (NOT caret-escaped): `^` is a literal, not an escape, between double-quotes, so
// a caret-escaped path would make cmd look for a filename that contains a caret
// and the daemon would never start. (Runtime-verified on real Windows: caret
// escaping broke the "Tom & Jerry" launch; plain quoting fixed it.)
func TestBuildLauncherVBSMetacharUsername(t *testing.T) {
	vbs := buildLauncherVBS(`C:\Users\Tom & Jerry (R&D)\unarr.exe`,
		`C:\Users\Tom & Jerry (R&D)\AppData\Local\unarr`)
	// The path must be present verbatim, quoted, un-caret-escaped.
	if !strings.Contains(vbs, `"C:\Users\Tom & Jerry (R&D)\unarr.exe"`) {
		t.Errorf("metachar path must be quoted verbatim (no caret escaping): %s", vbs)
	}
	if strings.Contains(vbs, "^&") || strings.Contains(vbs, "^(") {
		t.Errorf("path must NOT be caret-escaped — `^` is literal inside cmd quotes: %s", vbs)
	}
	// Verbatim path must appear in both the primary launch and its retry.
	if strings.Count(vbs, `"C:\Users\Tom & Jerry (R&D)\unarr.exe"`) < 2 {
		t.Errorf("path must appear in both launch and retry (got %d): %s",
			strings.Count(vbs, `"C:\Users\Tom & Jerry (R&D)\unarr.exe"`), vbs)
	}
}

// TestBuildWindowsTaskXMLBytes checks the on-disk encoding schtasks requires:
// UTF-16LE with a BOM. A wrong encoding is exactly what regressed task creation
// ("The task XML is malformed / unable to switch the encoding").
func TestBuildWindowsTaskXMLBytes(t *testing.T) {
	// Non-ASCII username exercises the UTF-16 path (would corrupt under UTF-8).
	data := serviceData{BinPath: `C:\unarr\unarr.exe`, User: `PC\José`}
	b := buildWindowsTaskXMLBytes(data, `C:\Users\José\AppData\Local\unarr`)

	if len(b) < 2 || b[0] != 0xFF || b[1] != 0xFE {
		t.Fatalf("missing UTF-16LE BOM (FF FE); first bytes: % x", b[:min(4, len(b))])
	}
	// Decode UTF-16LE back and confirm the declaration + the non-ASCII user.
	u16 := make([]uint16, 0, (len(b)-2)/2)
	for i := 2; i+1 < len(b); i += 2 {
		u16 = append(u16, uint16(b[i])|uint16(b[i+1])<<8)
	}
	text := string(utf16.Decode(u16))
	if !strings.Contains(text, `encoding="UTF-16"`) {
		t.Error("decoded XML does not declare UTF-16")
	}
	if !strings.Contains(text, `José`) {
		t.Error("non-ASCII username did not round-trip through UTF-16LE")
	}
}

// TestReregisterWindowsTaskNoopOffWindows guards the gate: on non-Windows the
// post-upgrade re-registration must be an inert no-op (no error, no rewrite) so
// wiring it into the shared self-update path never touches linux/macOS.
func TestReregisterWindowsTaskNoopOffWindows(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("gate is a no-op only off Windows; on Windows it may touch schtasks")
	}
	rewrote, err := reregisterWindowsTaskAfterUpgrade()
	if err != nil {
		t.Errorf("expected no error off Windows, got %v", err)
	}
	if rewrote {
		t.Error("must not rewrite a task off Windows")
	}
}

// ── Launcher exit code: the shim's respawn signal ────────────────────────────
//
// The scheduled task's RestartOnFailure is driven by the exit code of its
// ACTION — wscript.exe running this shim — not by the daemon's. A shim that
// falls off the end exits 0, so Task Scheduler always read "succeeded" and the
// respawn policy was dead code: a daemon that died right after logon stayed
// dead until the next one. These tests pin the two halves of the fix — that an
// exit code is always emitted, and that its value is decided by whether a stop
// was actually requested.

// TestBuildLauncherVBSAlwaysQuitsExplicitly is the core regression guard: the
// shim must never be able to end without setting an exit code.
func TestBuildLauncherVBSAlwaysQuitsExplicitly(t *testing.T) {
	vbs := buildLauncherVBS(`C:\unarr\unarr.exe`, `C:\Users\alice\AppData\Local\unarr`)

	if !strings.Contains(vbs, "WScript.Quit") {
		t.Fatalf("shim never sets an exit code — Task Scheduler will always see success:\n%s", vbs)
	}
	// Both verdicts must exist: 0 = the stop was asked for, stay down;
	// non-zero = it died, bring it back.
	if !strings.Contains(vbs, "WScript.Quit 0") {
		t.Errorf("no quit-0 path — a deliberate stop would be respawned:\n%s", vbs)
	}
	if !strings.Contains(vbs, "WScript.Quit 1") {
		t.Errorf("no quit-1 path — a crash would never be respawned:\n%s", vbs)
	}
	// The unguarded failure verdict must be the LAST statement, so every path
	// that is not an explicit "stopped on purpose" falls through to a respawn.
	lines := []string{}
	for _, l := range strings.Split(strings.TrimSpace(vbs), "\n") {
		if l = strings.TrimSpace(l); l != "" && !strings.HasPrefix(l, "'") {
			lines = append(lines, l)
		}
	}
	if last := lines[len(lines)-1]; last != "WScript.Quit 1" {
		t.Errorf("last statement is %q, want the fall-through `WScript.Quit 1`", last)
	}
}

// TestBuildLauncherVBSQuitsAfterTheDaemonExits guards the ordering: reading the
// marker and quitting must happen AFTER the (blocking) launch, never before.
func TestBuildLauncherVBSQuitsAfterTheDaemonExits(t *testing.T) {
	vbs := buildLauncherVBS(`C:\unarr\unarr.exe`, `C:\unarr`)

	lastRun := strings.LastIndex(vbs, "sh.Run")
	firstQuit := strings.Index(vbs, "WScript.Quit")
	if lastRun < 0 || firstQuit < 0 {
		t.Fatalf("shim is missing a launch or a quit:\n%s", vbs)
	}
	if firstQuit < lastRun {
		t.Errorf("shim quits before launching the daemon — it would exit at logon:\n%s", vbs)
	}
	// bWaitOnReturn must stay True, or the shim would quit while the daemon is
	// still starting and the task would report a bogus result immediately.
	if !strings.Contains(vbs, ", 0, True)") {
		t.Errorf("launch must stay hidden AND synchronous (0, True):\n%s", vbs)
	}
}

// TestBuildLauncherVBSEmbedsStopMarker pins the shim to the same marker path the
// daemon side writes (agent.StopIntentFileName, next to the state file). A drift
// here fails silently and in the worst direction: the shim would never see a
// requested stop and would resurrect a paused agent every minute.
func TestBuildLauncherVBSEmbedsStopMarker(t *testing.T) {
	logDir := `C:\Users\alice\AppData\Local\unarr`
	vbs := buildLauncherVBS(`C:\unarr\unarr.exe`, logDir)

	want := logDir + `\` + agent.StopIntentFileName
	if !strings.Contains(vbs, `"`+want+`"`) {
		t.Errorf("stop marker %q not embedded (quoted) in the shim:\n%s", want, vbs)
	}
	if !strings.Contains(vbs, "FileExists") {
		t.Errorf("shim does not test for the marker:\n%s", vbs)
	}
	// A trailing separator on the data dir must not produce a doubled one.
	vbs2 := buildLauncherVBS(`C:\unarr\unarr.exe`, logDir+`\`)
	if strings.Contains(vbs2, `\\`+agent.StopIntentFileName) {
		t.Errorf("doubled path separator before the marker:\n%s", vbs2)
	}
}

// TestBuildLauncherVBSMarkerPathQuotedForOddDataDir: the marker path goes
// through the same usernames as everything else here — spaces and cmd
// metacharacters — and lives inside a VBScript string literal.
func TestBuildLauncherVBSMarkerPathQuotedForOddDataDir(t *testing.T) {
	logDir := `C:\Users\Tom & Jerry (R&D)\AppData\Local\unarr`
	vbs := buildLauncherVBS(`C:\unarr\unarr.exe`, logDir)

	want := `"` + logDir + `\` + agent.StopIntentFileName + `"`
	if !strings.Contains(vbs, want) {
		t.Errorf("marker path not quoted verbatim for a metachar data dir:\n%s", vbs)
	}
	if strings.Contains(vbs, "^&") {
		t.Errorf("marker path must not be caret-escaped:\n%s", vbs)
	}
}

// TestBuildLauncherVBSSurvivesCreateObjectFailure: everything runs under
// On Error Resume Next, so a failed CreateObject must leave the script running
// all the way to its Quit rather than aborting into an implicit success.
func TestBuildLauncherVBSSurvivesCreateObjectFailure(t *testing.T) {
	vbs := buildLauncherVBS(`C:\unarr\unarr.exe`, `C:\unarr`)

	errIdx := strings.Index(vbs, "On Error Resume Next")
	createIdx := strings.Index(vbs, "CreateObject")
	if errIdx < 0 || createIdx < 0 {
		t.Fatalf("shim is missing error handling or object creation:\n%s", vbs)
	}
	if errIdx > createIdx {
		t.Errorf("On Error Resume Next must precede CreateObject, else a failure "+
			"aborts the script before it can set an exit code:\n%s", vbs)
	}
	// The marker check must be defended too: with fso == Nothing the shim has to
	// fall through to "respawn", never to an unhandled error.
	if !strings.Contains(vbs, "If Not fso Is Nothing Then") {
		t.Errorf("marker check is not guarded against a failed CreateObject:\n%s", vbs)
	}
}
