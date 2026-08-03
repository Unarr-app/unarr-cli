package cmd

import (
	"bytes"
	"encoding/binary"
	"strings"
	"unicode/utf16"

	"github.com/Unarr-app/unarr-cli/internal/agent"
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
// EXIT CODE: the shim MUST end with an explicit WScript.Quit. The scheduled
// task's RestartOnFailure is driven by the exit code of its action — which is
// wscript.exe, not the daemon — so a shim that just falls off the end always
// exits 0, Task Scheduler always reads "succeeded", and the respawn policy
// never fires. That is how a daemon that died seconds after logon stayed dead
// for days: nothing on Windows was ever going to restart it.
//
// The code is chosen from the stop-intent marker (agent.StopIntentFileName),
// not from the daemon's own exit status, because the status cannot answer the
// question: `unarr stop` is taskkill /f, so a user-initiated stop exits exactly
// like an AV kill, while the auto-upgrade exits 0 and DOES want a respawn.
// Marker present → the stop was asked for → quit 0 and stay down. Absent →
// quit non-zero → the task restarts the agent. Defaulting to "respawn" is
// deliberate: an unnecessary restart is a far smaller failure than an agent
// that is silently gone until the next logon.
//
// ENCODING: the returned script MUST be written to disk as UTF-16LE+BOM (see
// buildLauncherVBSBytes). Windows Script Host reads a BOM-less .vbs through the
// system ANSI code page, NOT UTF-8 — so a UTF-8 file whose paths contain a
// non-ASCII username (Zoë, André, 伊藤 …) would be mojibake'd and the daemon
// would silently never start at logon. This is the same lesson the task XML
// already encodes (schtasks likewise needs UTF-16). We build the string here
// and let buildLauncherVBSBytes do the UTF-16LE+BOM encoding, mirroring
// buildWindowsTaskXML / buildWindowsTaskXMLBytes.
func buildLauncherVBS(binPath, logDir string) string {
	logPath := strings.TrimRight(logDir, `\`) + `\unarr.log`

	// The command cmd.exe runs. Each path is wrapped in real double-quotes, which
	// is BOTH necessary and sufficient: quotes make spaces safe AND neutralise
	// cmd's metacharacters (& | < > ( )) — a Windows username can legally contain
	// them ("Tom & Jerry", "R&D"). Do NOT caret-escape inside the quotes: cmd
	// does not process `^` as an escape between double-quotes, so `Tom ^& Jerry`
	// would make it look for a file whose name literally contains a caret. The
	// nested-quote idiom `cmd /c ""prog" args"` is standard: cmd /c strips the
	// single outer pair, leaving each path individually quoted and protected.
	cmdFor := func(withRedirect bool) string {
		if withRedirect {
			return `cmd /c ""` + binPath + `" start >> "` + logPath + `" 2>&1"`
		}
		return `cmd /c ""` + binPath + `" start"`
	}

	// Marker the shim consults to tell a requested stop from a death. Built from
	// logDir (the data dir the installer just wrote the shim into) rather than
	// resolved at run time — by the time this code runs the daemon is gone.
	stopMarker := strings.TrimRight(logDir, `\`) + `\` + agent.StopIntentFileName

	// Run(cmd, 0, True): window style 0 = hidden, bWaitOnReturn = True so we can
	// read the exit code and retry. The daemon blocks for the life of the agent,
	// so this shim (and the wscript host) stays resident for as long as the
	// daemon runs — which is what keeps the scheduled task in the Running state.
	var b strings.Builder
	b.WriteString("' unarr daemon launcher — generated by `unarr daemon install`.\n")
	b.WriteString("' Runs the console-subsystem daemon with no console window (see daemon_launch_vbs.go).\n")
	// On Error Resume Next FIRST: a failed CreateObject must not abort the script
	// before the Quit at the bottom, or the task would again see a bare success.
	b.WriteString("On Error Resume Next\n")
	b.WriteString("Set sh = CreateObject(\"WScript.Shell\")\n")
	b.WriteString("Set fso = CreateObject(\"Scripting.FileSystemObject\")\n")
	b.WriteString("rc = sh.Run(" + vbsQuote(cmdFor(true)) + ", 0, True)\n")
	// Retry once, still redirected, if the first launch could not run at all
	// (Err set) — a genuinely failed start should still leave a log line, not
	// vanish. A non-zero rc from a daemon that DID run and later exited is not a
	// launch failure, so don't relaunch on rc alone (that would blindly restart
	// a crashed daemon inside the shim; the task's RestartOnFailure owns
	// recovery, with a backoff and an attempt cap this loop would not have).
	b.WriteString("If Err.Number <> 0 Then\n")
	b.WriteString("  Err.Clear\n")
	b.WriteString("  rc = sh.Run(" + vbsQuote(cmdFor(true)) + ", 0, True)\n")
	b.WriteString("End If\n")
	// Translate "was this stop asked for?" into the task's respawn signal. fso is
	// Nothing only if CreateObject failed; treat that as "not stopped on purpose"
	// so the agent still comes back — the safe direction (see the doc comment).
	b.WriteString("Err.Clear\n")
	b.WriteString("stopped = False\n")
	b.WriteString("If Not fso Is Nothing Then\n")
	b.WriteString("  stopped = fso.FileExists(" + vbsQuote(stopMarker) + ")\n")
	b.WriteString("End If\n")
	b.WriteString("If Err.Number <> 0 Then stopped = False\n")
	b.WriteString("If stopped Then\n")
	b.WriteString("  WScript.Quit 0\n")
	b.WriteString("End If\n")
	b.WriteString("WScript.Quit 1\n")
	return b.String()
}

// buildLauncherVBSBytes encodes the shim as UTF-16LE with a BOM — the encoding
// Windows Script Host needs to read non-ASCII paths correctly (a BOM-less file
// is decoded via the ANSI code page). Mirrors buildWindowsTaskXMLBytes.
func buildLauncherVBSBytes(binPath, logDir string) []byte {
	s := buildLauncherVBS(binPath, logDir)
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
