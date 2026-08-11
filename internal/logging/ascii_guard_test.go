package logging_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestLogLinesAreASCII is an architectural regression guard: no string literal
// handed to a log.* call — or to fmt.Fprint* on os.Stderr — may contain a
// non-ASCII byte.
//
// WHY THIS IS NOT PEDANTRY. Those two destinations are the daemon's log files
// (unarr.log via log.SetOutput, unarr.boot.log via the launcher's stderr
// redirect), and those files are read back by at least four things that do NOT
// all assume UTF-8:
//
//   - `unarr logs` printing to a Windows console, whose code page is 850/437
//     ("ΓÇö" — observed on the VM harness);
//   - Notepad and friends opening the support-bundle dump, BOM-less;
//   - the crash-report pipeline, which delivered "windows ?" install
//     cloudflared" to the developers instead of an em dash;
//   - PowerShell 5.1, which decodes BOM-less UTF-8 as CP1252 — the same fault
//     that turned one em dash in a .ps1 string into a whole-file parse error.
//
// A log line's job is to be legible in the worst reader that will ever open it.
// ASCII is legible in all of them. Terminal UI (the ✓/⚠ in fmt.Println output,
// the box-drawing in internal/ui) is deliberately NOT covered: that goes to an
// interactive terminal the user is looking at, not to a file another program
// decodes.
func TestLogLinesAreASCII(t *testing.T) {
	root := findRepoRoot(t)
	fset := token.NewFileSet()
	var findings []string

	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			switch filepath.Base(path) {
			case ".git", "graphify-out", "dist", "node_modules", "worktrees", "shared", "testdata":
				return filepath.SkipDir
			}
			return nil
		}
		// Tests may say what they like: their output goes to a terminal or a CI
		// log, never to unarr.log.
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		src, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		f, parseErr := parser.ParseFile(fset, path, src, 0)
		if parseErr != nil {
			return nil // not our business to fail on unparseable files
		}
		rel, _ := filepath.Rel(root, path)
		report := func(lit *ast.BasicLit) {
			findings = append(findings, rel+":"+
				itoa(fset.Position(lit.Pos()).Line)+": "+firstNonASCII(lit.Value)+" in "+trim(lit.Value))
		}
		logged := loggedIdents(f)
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			// Direct: the literal is an argument of the log call itself.
			if writesToALogFile(call) {
				for _, arg := range call.Args {
					ast.Inspect(arg, func(m ast.Node) bool {
						if lit, ok := m.(*ast.BasicLit); ok && lit.Kind == token.STRING && !isASCII(lit.Value) {
							report(lit)
						}
						return true
					})
				}
			}
			return true
		})
		// Indirect: fmt.Sprintf into a variable that a log call later prints.
		//
		// This is not a hypothetical hole. The torrent progress line is built
		// exactly this way — `line := fmt.Sprintf("[%s] %d%% — …")` followed by
		// `log.Print(line)` — and its em dash reached users' log files, and the
		// crash reports, for as long as this guard has existed. Measured on the
		// Windows VM harness: it arrives as the bytes C7 F6 under code page 437.
		//
		// Walked over ASSIGNMENTS rather than calls because that is where the
		// variable's name is; a CallExpr on its own cannot say where its value ends
		// up.
		ast.Inspect(f, func(n ast.Node) bool {
			assign, ok := n.(*ast.AssignStmt)
			if !ok {
				return true
			}
			for i, rhs := range assign.Rhs {
				call, ok := rhs.(*ast.CallExpr)
				if !ok || !isSprintCall(call) || i >= len(assign.Lhs) {
					continue
				}
				id, ok := assign.Lhs[i].(*ast.Ident)
				if !ok || !logged[id.Name] {
					continue
				}
				for _, arg := range call.Args {
					if lit, ok := arg.(*ast.BasicLit); ok && lit.Kind == token.STRING && !isASCII(lit.Value) {
						report(lit)
					}
				}
			}
			return true
		})
		// Indirect, second form: a strings.Builder assembled in one function and
		// logged from ANOTHER file.
		//
		// HWAccelDiagnostic.LogLine() is built this way — a chain of
		// b.WriteString(...) in internal/engine/hwaccel.go — and printed by a
		// log.Printf in a different file. Neither of the two passes above can see
		// it: the literal is not an argument of a log call, and it is not assigned
		// to a variable this file logs. Its em dash reached production, and the
		// Windows VM harness caught it as the bytes C7 F6 in a real daemon log
		// AFTER the first two passes were already green.
		//
		// Cross-file call graphs are out of scope for a guard that parses one file
		// at a time, so this keys on the shape every log line in this repo has: a
		// "[tag] " prefix somewhere in the builder. That is what makes a string a
		// log line here, and it is why builders that assemble terminal UI (which is
		// exempt) do not trip it.
		if buildsALogLine(f) {
			ast.Inspect(f, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok || !isBuilderWrite(call) {
					return true
				}
				for _, arg := range call.Args {
					if lit, ok := arg.(*ast.BasicLit); ok && lit.Kind == token.STRING && !isASCII(lit.Value) {
						report(lit)
					}
				}
				return true
			})
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if len(findings) > 0 {
		t.Fatalf("non-ASCII in %d log literal(s) — use '-' for an em dash, '->' for an arrow, "+
			"'...' for an ellipsis:\n  %s", len(findings), strings.Join(findings, "\n  "))
	}
}

// writesToALogFile reports whether this call's output lands in a log file:
// any log.* call, or fmt.Fprint*/Fprintf on os.Stderr, which every supervised
// launcher redirects into unarr.boot.log.
func writesToALogFile(call *ast.CallExpr) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	if !ok {
		return false
	}
	switch pkg.Name {
	case "log":
		return true
	case "fmt":
		if !strings.HasPrefix(sel.Sel.Name, "Fprint") || len(call.Args) == 0 {
			return false
		}
		w, ok := call.Args[0].(*ast.SelectorExpr)
		if !ok {
			return false
		}
		x, ok := w.X.(*ast.Ident)
		return ok && x.Name == "os" && w.Sel.Name == "Stderr"
	}
	return false
}

// loggedIdents collects the names of variables this file ever hands to a log
// call — `log.Print(line)`, `log.Printf("%s", msg)`, `fmt.Fprintln(os.Stderr, s)`.
//
// One flat set per file, with no scope tracking, and that is the deliberate
// trade: a guard that misses a real mojibake bug is worthless, while a guard
// that occasionally asks for ASCII in a same-named local that is not logged
// costs one substitution. It over-approximates on purpose. Shadowing in Go is
// common enough that name collisions happen; '-' instead of an em dash is never
// the wrong answer in a string that might reach a log file.
func loggedIdents(f *ast.File) map[string]bool {
	out := map[string]bool{}
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || !writesToALogFile(call) {
			return true
		}
		for _, arg := range call.Args {
			if id, ok := arg.(*ast.Ident); ok {
				out[id.Name] = true
			}
		}
		return true
	})
	return out
}

// buildsALogLine reports whether this file assembles something with the shape of
// a log line: a "[tag] " prefix written into a builder or Sprint call.
//
// That prefix is the convention every log line in this repo follows —
// "[transcode] ", "[funnel] ", "[torrent] " — and it is a far better signal than
// the function's name. A builder producing terminal UI (internal/ui, the doctor
// renderers) never writes one, which is exactly why those stay exempt.
func buildsALogLine(f *ast.File) bool {
	found := false
	ast.Inspect(f, func(n ast.Node) bool {
		if found {
			return false
		}
		lit, ok := n.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return true
		}
		// lit.Value keeps its quotes; a tag prefix looks like `"[name] ` or
		// `"[name]"`, always at the very start of the literal.
		v := lit.Value
		if len(v) < 4 || (v[0] != '"' && v[0] != '`') || v[1] != '[' {
			return true
		}
		if i := strings.IndexByte(v, ']'); i > 2 {
			found = true
			return false
		}
		return true
	})
	return found
}

// isBuilderWrite reports whether this call is x.WriteString(...) — the
// strings.Builder / bytes.Buffer idiom for assembling a line piece by piece.
func isBuilderWrite(call *ast.CallExpr) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	return ok && sel.Sel.Name == "WriteString" && len(call.Args) > 0
}

// isSprintCall reports whether this call is fmt.Sprint/Sprintf/Sprintln — the
// three ways a log line gets BUILT before something else prints it.
func isSprintCall(call *ast.CallExpr) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	return ok && pkg.Name == "fmt" && strings.HasPrefix(sel.Sel.Name, "Sprint")
}

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] > 0x7F {
			return false
		}
	}
	return true
}

func firstNonASCII(s string) string {
	for _, r := range s {
		if r > 0x7F {
			return string(r)
		}
	}
	return ""
}

func trim(s string) string {
	if len(s) > 70 {
		return s[:70] + `…"`
	}
	return s
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

func findRepoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Skip("go.mod not found walking up: sources are not deployed next to this binary")
		}
		dir = parent
	}
}
