//go:build linux

package sysinfo

import (
	"os"
	"path/filepath"
	"testing"
)

func TestTotalMemoryKnown(t *testing.T) {
	if total, ok := TotalMemory(); !ok || total < 64<<20 {
		t.Fatalf("TotalMemory = %d, %v; want at least 64 MiB", total, ok)
	}
}

func TestTotalMemoryHonoursCgroupLimit(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	old := cgroupMemoryLimitPaths
	t.Cleanup(func() { cgroupMemoryLimitPaths = old })

	cgroupMemoryLimitPaths = []string{write("v2-max", "max\n"), write("v1", "536870912\n")}
	if total, ok := TotalMemory(); !ok || total != 512<<20 {
		t.Fatalf("with a 512 MiB v1 limit: TotalMemory = %d, %v", total, ok)
	}
	cgroupMemoryLimitPaths = []string{write("v2-huge", "9223372036854771712\n")}
	if total, ok := TotalMemory(); !ok || total == 9223372036854771712 {
		t.Fatalf("an unlimited cgroup must not raise total memory: %d, %v", total, ok)
	}
}
