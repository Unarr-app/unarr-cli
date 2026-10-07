package engine

import "testing"

func TestStreamServer_ClearFileIfOnlyClearsItsGeneration(t *testing.T) {
	srv := NewStreamServer(0, 1)
	g1 := srv.SetFile(newFakeProvider("a.mkv", []byte("aaaa")), "task-a")
	g2 := srv.SetFile(newFakeProvider("b.mkv", []byte("bbbb")), "task-b")
	if g2 <= g1 || g1 == 0 {
		t.Fatalf("generations not increasing from 1: g1=%d g2=%d", g1, g2)
	}
	if srv.ClearFileIf(g1) {
		t.Fatal("a stale generation must not clear the newer file")
	}
	if !srv.HasFile() || srv.CurrentTaskID() != "task-b" {
		t.Fatalf("newer file lost (has=%v task=%q)", srv.HasFile(), srv.CurrentTaskID())
	}
	if !srv.ClearFileIf(g2) || srv.HasFile() {
		t.Fatal("the current generation must clear")
	}
	if srv.ClearFileIf(g2) {
		t.Fatal("clearing bumps the generation: a second clear is a no-op")
	}
}
