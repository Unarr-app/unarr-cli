package doctor

import (
	"bytes"
	"encoding/json"
	"errors"
	"testing"
)

func TestRunClassifiesAndCounts(t *testing.T) {
	specs := []Spec{
		{Group: "G", Name: "pass", Fn: func() (string, error) { return "fine", nil }},
		{Group: "G", Name: "warn", Fn: func() (string, error) { return "!careful", nil }, Remedy: "do X"},
		{Group: "G", Name: "fail", Fn: func() (string, error) { return "broken", errors.New("nope") }, Remedy: "do Y"},
		// An error wins over the '!' prefix: a check that failed is not a warning.
		{Group: "G", Name: "both", Fn: func() (string, error) { return "!hint", errors.New("nope") }},
		{Group: "G", Name: "quiet pass", Fn: func() (string, error) { return "", nil }, Remedy: "unused"},
	}

	rep := Run(specs, nil)

	if rep.Passed != 2 || rep.Warned != 1 || rep.Failed != 2 {
		t.Fatalf("counts = %d/%d/%d, want 2 passed, 1 warned, 2 failed", rep.Passed, rep.Warned, rep.Failed)
	}
	if rep.Status != StatusFail {
		t.Errorf("overall status = %q, want %q", rep.Status, StatusFail)
	}

	want := []Check{
		{Group: "G", Name: "pass", Status: StatusPass, Message: "fine"},
		{Group: "G", Name: "warn", Status: StatusWarn, Message: "careful", Remedy: "do X"},
		{Group: "G", Name: "fail", Status: StatusFail, Message: "broken", Remedy: "do Y", Error: "nope"},
		{Group: "G", Name: "both", Status: StatusFail, Message: "!hint", Error: "nope"},
		{Group: "G", Name: "quiet pass", Status: StatusPass},
	}
	if len(rep.Checks) != len(want) {
		t.Fatalf("got %d checks, want %d", len(rep.Checks), len(want))
	}
	for i, w := range want {
		if rep.Checks[i] != w {
			t.Errorf("check %d = %+v, want %+v", i, rep.Checks[i], w)
		}
	}
}

func TestRunOverallStatusWarn(t *testing.T) {
	rep := Run([]Spec{
		{Group: "G", Name: "a", Fn: func() (string, error) { return "", nil }},
		{Group: "G", Name: "b", Fn: func() (string, error) { return "!eh", nil }},
	}, nil)
	if rep.Status != StatusWarn {
		t.Errorf("status = %q, want %q", rep.Status, StatusWarn)
	}
}

func TestRunEmptyReportsPass(t *testing.T) {
	rep := Run(nil, nil)
	if rep.Status != StatusPass || rep.Checks == nil {
		t.Errorf("empty run = %+v, want pass with a non-nil (JSON `[]`) check list", rep)
	}
}

func TestRenderJSONShape(t *testing.T) {
	rep := Run([]Spec{
		{Group: "Config", Name: "Config file", Fn: func() (string, error) { return "/tmp/c.toml", nil }},
		{Group: "Config", Name: "API key", Fn: func() (string, error) { return "missing", errors.New("no key") }, Remedy: "run `unarr init`"},
	}, nil)

	var buf bytes.Buffer
	if err := RenderJSON(&buf, rep); err != nil {
		t.Fatalf("RenderJSON: %v", err)
	}

	var got map[string]any
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, buf.String())
	}
	if got["status"] != "fail" || got["failed"] != float64(1) || got["passed"] != float64(1) {
		t.Errorf("totals wrong: %v", got)
	}

	checks, ok := got["checks"].([]any)
	if !ok || len(checks) != 2 {
		t.Fatalf("checks = %v, want 2 entries", got["checks"])
	}
	second, _ := checks[1].(map[string]any)
	for key, want := range map[string]any{
		"group":   "Config",
		"name":    "API key",
		"status":  "fail",
		"message": "missing",
		"remedy":  "run `unarr init`",
		"error":   "no key",
	} {
		if second[key] != want {
			t.Errorf("checks[1].%s = %v, want %v", key, second[key], want)
		}
	}
	// A passing check carries no remedy/error noise.
	first, _ := checks[0].(map[string]any)
	if _, ok := first["remedy"]; ok {
		t.Errorf("passing check leaked a remedy: %v", first)
	}
}
