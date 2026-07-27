package winproc_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestEveryExecCommandHidesWindow is an architectural regression guard.
//
// On Windows, any exec.Command / exec.CommandContext of a console-subsystem
// child spawned from the -H=windowsgui tray or a windowless daemon allocates a
// fresh console window that flashes (see package doc). Every such call site
// MUST route its *exec.Cmd through winproc.HideWindow (or an equivalent helper
// that sets CREATE_NO_WINDOW). This test fails if a new exec site is added in a
// guarded package without that call — the exact failure mode that once left the
// Linux-only hardenCmd applied at 1 of 15 mediainfo sites.
//
// Detection is intentionally coarse: within the function enclosing an
// exec.Command call, SOME window-suppressing helper must be referenced. A false
// negative (two exec calls in one function, only one wrapped) is possible but
// unlikely and cheap to catch in review; the guard's job is to stop a whole
// site being forgotten.
func TestEveryExecCommandHidesWindow(t *testing.T) {
	repoRoot := findRepoRoot(t)

	// Packages whose exec.Command sites must suppress the console window.
	guardedDirs := []string{
		"internal/engine",
		"internal/library/mediainfo",
		"internal/usenet/postprocess",
		"internal/notify",
		"internal/dialog",
		"internal/arr",
		"internal/cmd",
		"cmd/unarr-desktop",
	}

	// Helpers that count as "window suppressed" when referenced in the same
	// function as the exec.Command call.
	suppressors := []string{
		"HideWindow",          // winproc.HideWindow — the canonical wrapper
		"detachedSysProcAttr", // daemon fork: DETACHED_PROCESS|CREATE_NO_WINDOW
		"SysProcAttr",         // an explicit inline SysProcAttr with the flag
	}

	// Exact "file:funcName" sites that are deliberately exempt (document why).
	// Add here only with a reason.
	exempt := map[string]bool{
		// OS-gated by filename suffix (_darwin.go / _linux.go) — never compiled
		// on Windows, so their exec.Command cannot allocate a Windows console.
		"cmd/unarr-desktop/playersystem_darwin.go:defaultMovieBundleID":    true,
		"cmd/unarr-desktop/playersystem_linux.go:defaultVideoDesktopEntry": true,
	}

	fset := token.NewFileSet()
	var violations []string

	for _, dir := range guardedDirs {
		absDir := filepath.Join(repoRoot, dir)
		entries, err := os.ReadDir(absDir)
		if err != nil {
			t.Fatalf("read guarded dir %s: %v", dir, err)
		}
		for _, e := range entries {
			name := e.Name()
			if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
				continue
			}
			path := filepath.Join(absDir, name)
			f, err := parser.ParseFile(fset, path, nil, 0)
			if err != nil {
				t.Fatalf("parse %s: %v", path, err)
			}
			for _, decl := range f.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || fn.Body == nil {
					continue
				}
				if !callsExecCommand(fn.Body) {
					continue
				}
				key := dir + "/" + name + ":" + fn.Name.Name
				if exempt[key] {
					continue
				}
				if referencesAny(fn.Body, suppressors) {
					continue
				}
				violations = append(violations, key)
			}
		}
	}

	if len(violations) > 0 {
		t.Fatalf("exec.Command/CommandContext without a window suppressor "+
			"(call winproc.HideWindow(cmd) before running the child) in:\n  %s",
			strings.Join(violations, "\n  "))
	}
}

// callsExecCommand reports whether the body contains an exec.Command or
// exec.CommandContext selector call.
func callsExecCommand(body *ast.BlockStmt) bool {
	found := false
	ast.Inspect(body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "exec" &&
			(sel.Sel.Name == "Command" || sel.Sel.Name == "CommandContext") {
			found = true
			return false
		}
		return true
	})
	return found
}

// referencesAny reports whether any of the given identifier names appears
// anywhere in the body (as a selector .Name or a bare ident).
func referencesAny(body *ast.BlockStmt, names []string) bool {
	want := make(map[string]bool, len(names))
	for _, n := range names {
		want[n] = true
	}
	found := false
	ast.Inspect(body, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.Ident:
			if want[x.Name] {
				found = true
				return false
			}
		case *ast.SelectorExpr:
			if want[x.Sel.Name] {
				found = true
				return false
			}
		}
		return true
	})
	return found
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
			t.Fatal("go.mod not found walking up from test dir")
		}
		dir = parent
	}
}
