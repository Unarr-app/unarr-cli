// Package doctor turns the `unarr doctor` diagnostics into data: a list of
// specs is executed into a report, and renderers (console, JSON) consume that
// report. Keeping the checks separate from their presentation is what makes
// `--json` — and, later, support-bundle and the Docker HEALTHCHECK — possible.
//
// The package deliberately depends on nothing but the stdlib and the color
// library: the concrete check bodies live in internal/cmd (they need its
// config/client helpers), so importing internal/cmd from here would be a cycle.
package doctor

// Status is the outcome of a single check. The string values are the JSON
// contract consumers filter on (`jq -e '.status == "pass"'`), so they must not
// change casing or spelling.
type Status string

const (
	StatusPass Status = "pass"
	StatusWarn Status = "warn"
	StatusFail Status = "fail"
)

// Check is one executed diagnostic. Message is the human sentence the console
// prints; Remedy and Error are extra detail only machine consumers read (a
// remedy shown in the web health panel, the raw error for a support bundle) —
// the text renderer ignores them so its output stays byte-stable.
type Check struct {
	Group   string `json:"group"`
	Name    string `json:"name"`
	Status  Status `json:"status"`
	Message string `json:"message"`
	Remedy  string `json:"remedy,omitempty"`
	Error   string `json:"error,omitempty"`
}

// Spec is a check before it runs. Fn keeps the historical `(string, error)`
// convention of runDoctor's check() closure: a non-nil error is a FAIL, a
// message starting with '!' is a WARN (the '!' is stripped), anything else is
// a PASS. Remedy is optional and is surfaced only when the check does not pass.
type Spec struct {
	Group  string
	Name   string
	Fn     func() (string, error)
	Remedy string
}

// classify applies the (string, error) convention. An error always wins over
// the '!' prefix: a check that both failed and hinted is a failure.
func classify(msg string, err error) (Status, string) {
	switch {
	case err != nil:
		return StatusFail, msg
	case msg != "" && msg[0] == '!':
		return StatusWarn, msg[1:]
	default:
		return StatusPass, msg
	}
}
