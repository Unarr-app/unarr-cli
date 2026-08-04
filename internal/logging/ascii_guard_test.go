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
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || !writesToALogFile(call) {
				return true
			}
			for _, arg := range call.Args {
				ast.Inspect(arg, func(m ast.Node) bool {
					lit, ok := m.(*ast.BasicLit)
					if !ok || lit.Kind != token.STRING || isASCII(lit.Value) {
						return true
					}
					findings = append(findings, rel+":"+
						itoa(fset.Position(lit.Pos()).Line)+": "+firstNonASCII(lit.Value)+" in "+trim(lit.Value))
					return true
				})
			}
			return true
		})
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
