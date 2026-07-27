package cmd

import (
	"encoding/xml"
	"strings"
	"testing"
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

	// The daemon binary and its `start` verb must reach the action.
	if !strings.Contains(rt.Actions.Exec.Arguments, data.BinPath) {
		t.Errorf("action arguments missing bin path: %q", rt.Actions.Exec.Arguments)
	}
	if !strings.Contains(rt.Actions.Exec.Arguments, "start") {
		t.Errorf("action arguments missing `start` verb: %q", rt.Actions.Exec.Arguments)
	}

	// Regression: Start-Transcript must be best-effort (wrapped), never the old
	// -NoClobber form that aborts the whole launch when the log is locked.
	if strings.Contains(rt.Actions.Exec.Arguments, "-NoClobber") {
		t.Error("Start-Transcript -NoClobber can abort launch before the daemon starts")
	}
	if !strings.Contains(rt.Actions.Exec.Arguments, "try {") {
		t.Error("Start-Transcript is not wrapped in try/catch — a log failure can stop the agent")
	}

	// The runtime user must be carried into both the principal and trigger.
	if rt.Principals.Principal.UserID != data.User {
		t.Errorf("principal UserId = %q, want %q", rt.Principals.Principal.UserID, data.User)
	}
}
