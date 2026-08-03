package doctor

// Report is the full outcome of a doctor run and the shape emitted by --json.
// Status is the worst individual status, so a HEALTHCHECK can decide with a
// single jq expression instead of folding over Checks.
type Report struct {
	Status Status  `json:"status"`
	Checks []Check `json:"checks"`
	Passed int     `json:"passed"`
	Warned int     `json:"warned"`
	Failed int     `json:"failed"`
}

// Run executes specs in order and returns the aggregated report.
//
// onCheck (may be nil) fires the moment each check completes. That callback is
// not a convenience: the connectivity checks take up to 10 s each, so the
// console renderer must print live — buffering the whole run would leave
// `unarr doctor` looking hung for half a minute. The JSON renderer passes nil
// and prints the report once at the end.
func Run(specs []Spec, onCheck func(Check)) Report {
	rep := Report{Status: StatusPass, Checks: make([]Check, 0, len(specs))}
	for _, spec := range specs {
		c := run1(spec)
		switch c.Status {
		case StatusFail:
			rep.Failed++
		case StatusWarn:
			rep.Warned++
		default:
			rep.Passed++
		}
		rep.Status = worst(rep.Status, c.Status)
		rep.Checks = append(rep.Checks, c)
		if onCheck != nil {
			onCheck(c)
		}
	}
	return rep
}

// run1 executes a single spec. A remedy on a passing check is noise, so it is
// dropped — consumers can treat a non-empty Remedy as "there is something to do".
func run1(spec Spec) Check {
	msg, err := spec.Fn()
	status, text := classify(msg, err)
	c := Check{Group: spec.Group, Name: spec.Name, Status: status, Message: text}
	if err != nil {
		c.Error = err.Error()
	}
	if status != StatusPass {
		c.Remedy = spec.Remedy
	}
	return c
}

// worst returns the more severe of two statuses (fail > warn > pass).
func worst(a, b Status) Status {
	if severity(b) > severity(a) {
		return b
	}
	return a
}

func severity(s Status) int {
	switch s {
	case StatusFail:
		return 2
	case StatusWarn:
		return 1
	default:
		return 0
	}
}
