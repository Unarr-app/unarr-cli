package cmd

import (
	"bytes"
	"encoding/binary"
	"encoding/xml"
	"fmt"
	"strings"
	"unicode/utf16"
)

// logonDelaySeconds is how long the scheduled task waits after logon before
// launching the daemon, giving the network/VPN stack time to come up so the
// agent's first server reach doesn't race a not-yet-ready network.
const logonDelaySeconds = 20

// buildWindowsTaskXML renders a Task Scheduler 1.4 definition for the unarr
// daemon. Compared to the old `schtasks /create` flag form it adds a logon
// delay, restart-on-failure, and start-when-available — the settings that make
// login start-up reliable. It is invoked with `schtasks /create /xml`.
//
// The action launches a VBScript shim via wscript.exe rather than the daemon
// (or a `powershell -WindowStyle Hidden` wrapper) directly. unarr.exe is a
// console-subsystem binary, so any launcher that hosts a console — including a
// hidden PowerShell — makes Task Scheduler allocate and momentarily draw a
// console window at logon before it can be hidden: the flash the user saw
// before the desktop app started. wscript.exe is GUI-subsystem (never allocates
// a console) and its WshShell.Run(cmd, 0, …) starts the daemon fully headless.
// The shim keeps log capture and the no-log fallback (see daemon_launch_vbs.go).
// buildWindowsTaskXMLBytes renders the task definition and encodes it as
// UTF-16LE with a BOM — what `schtasks /create /xml` actually requires. The
// prolog declares UTF-16 to match. (An earlier attempt declared UTF-8 with a
// UTF-16 prolog and vice-versa; schtasks reads the declared encoding and
// rejects a mismatch with "The task XML is malformed / unable to switch the
// encoding". UTF-16LE+BOM + a UTF-16 declaration is the combination it accepts,
// and it also carries a non-ASCII Windows username correctly.)
func buildWindowsTaskXMLBytes(data serviceData, logDir string) []byte {
	s := buildWindowsTaskXML(data, logDir)
	u16 := utf16.Encode([]rune(s))
	buf := &bytes.Buffer{}
	buf.Write([]byte{0xFF, 0xFE}) // UTF-16LE BOM
	for _, r := range u16 {
		_ = binary.Write(buf, binary.LittleEndian, r)
	}
	return buf.Bytes()
}

func buildWindowsTaskXML(data serviceData, logDir string) string {
	// The action runs the VBScript shim through wscript.exe (GUI-subsystem, no
	// console). installWindowsTask writes the shim to this same path before
	// creating the task. Quote it: the data dir contains the Windows username
	// and can hold spaces.
	vbsPath := strings.TrimRight(logDir, `\`) + `\` + launcherVBSName
	command := "wscript.exe"
	arguments := fmt.Sprintf(`"%s"`, vbsPath)

	delay := fmt.Sprintf("PT%dS", logonDelaySeconds)

	t := taskXML{
		Version: "1.4",
		Xmlns:   "http://schemas.microsoft.com/windows/2004/02/mit/task",
		RegistrationInfo: regInfo{
			Description: "unarr download daemon",
			URI:         `\unarr`,
		},
		Triggers: triggers{Logon: logonTrigger{
			Enabled: true,
			UserID:  data.User,
			Delay:   delay,
		}},
		Principals: principals{Principal: principal{
			ID:        "Author",
			UserID:    data.User,
			LogonType: "InteractiveToken",
			RunLevel:  "LeastPrivilege",
		}},
		Settings: taskSettings{
			MultipleInstancesPolicy:    "IgnoreNew",
			DisallowStartIfOnBatteries: false,
			StopIfGoingOnBatteries:     false,
			StartWhenAvailable:         true,
			AllowStartOnDemand:         true,
			Enabled:                    true,
			// Supervisor: retry a failed/early-exited start 3× at 1-minute
			// spacing. This is the systemd-Restart equivalent the flag form
			// lacked. ExecutionTimeLimit PT0S = run forever (it's a daemon).
			RestartOnFailure:   &restartOnFailure{Interval: "PT1M", Count: 3},
			ExecutionTimeLimit: "PT0S",
			Priority:           7,
		},
		Actions: actions{Context: "Author", Exec: execAction{
			Command:   command,
			Arguments: arguments,
		}},
	}

	out, err := xml.MarshalIndent(t, "", "  ")
	if err != nil {
		// The struct is static-shaped; marshal cannot realistically fail. Fall
		// back to a minimal valid doc rather than returning an error up a call
		// path that has none.
		return `<?xml version="1.0" encoding="UTF-16"?>` + "\n" +
			`<Task version="1.4" xmlns="http://schemas.microsoft.com/windows/2004/02/mit/task"/>`
	}
	// UTF-16 prolog: buildWindowsTaskXMLBytes writes these characters as
	// UTF-16LE+BOM, which is the encoding schtasks /create /xml requires (it
	// reads the declared encoding and rejects a mismatch as "task XML is
	// malformed / unable to switch the encoding").
	return `<?xml version="1.0" encoding="UTF-16"?>` + "\n" + string(out)
}

type taskXML struct {
	XMLName          xml.Name     `xml:"Task"`
	Version          string       `xml:"version,attr"`
	Xmlns            string       `xml:"xmlns,attr"`
	RegistrationInfo regInfo      `xml:"RegistrationInfo"`
	Triggers         triggers     `xml:"Triggers"`
	Principals       principals   `xml:"Principals"`
	Settings         taskSettings `xml:"Settings"`
	Actions          actions      `xml:"Actions"`
}

type regInfo struct {
	Description string `xml:"Description"`
	URI         string `xml:"URI"`
}

type triggers struct {
	Logon logonTrigger `xml:"LogonTrigger"`
}

type logonTrigger struct {
	Enabled bool   `xml:"Enabled"`
	UserID  string `xml:"UserId,omitempty"`
	Delay   string `xml:"Delay,omitempty"`
}

type principals struct {
	Principal principal `xml:"Principal"`
}

type principal struct {
	ID        string `xml:"id,attr"`
	UserID    string `xml:"UserId"`
	LogonType string `xml:"LogonType"`
	RunLevel  string `xml:"RunLevel"`
}

// Field order MATTERS: encoding/xml emits elements in struct-field order, and
// the Task Scheduler settingsType XSD fixes the child-element sequence. A wrong
// order imports fine on a lenient Windows 11 host but can be rejected by a
// stricter validator (GPO-locked / future OS). This is the XSD sequence.
type taskSettings struct {
	AllowStartOnDemand         bool              `xml:"AllowStartOnDemand"`
	RestartOnFailure           *restartOnFailure `xml:"RestartOnFailure,omitempty"`
	MultipleInstancesPolicy    string            `xml:"MultipleInstancesPolicy"`
	DisallowStartIfOnBatteries bool              `xml:"DisallowStartIfOnBatteries"`
	StopIfGoingOnBatteries     bool              `xml:"StopIfGoingOnBatteries"`
	StartWhenAvailable         bool              `xml:"StartWhenAvailable"`
	ExecutionTimeLimit         string            `xml:"ExecutionTimeLimit"`
	Priority                   int               `xml:"Priority"`
	Enabled                    bool              `xml:"Enabled"`
}

type restartOnFailure struct {
	Interval string `xml:"Interval"`
	Count    int    `xml:"Count"`
}

type actions struct {
	Context string     `xml:"Context,attr"`
	Exec    execAction `xml:"Exec"`
}

type execAction struct {
	Command   string `xml:"Command"`
	Arguments string `xml:"Arguments"`
}
