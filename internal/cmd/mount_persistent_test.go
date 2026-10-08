package cmd

import "testing"

func TestUmountCommandAlsoAcceptsUnmount(t *testing.T) {
	cmd := newUmountCmd()
	if cmd.Use != "umount" || len(cmd.Aliases) != 1 || cmd.Aliases[0] != "unmount" {
		t.Fatalf("unexpected unmount command: use=%q aliases=%v", cmd.Use, cmd.Aliases)
	}
}

func TestMountCommandDescribesPersistentLifecycle(t *testing.T) {
	cmd := newMountCmd()
	if cmd.Short == "" {
		t.Fatal("mount command needs user-facing lifecycle help")
	}
	foundServe := false
	for _, child := range cmd.Commands() {
		if child.Name() == "serve" {
			foundServe = true
		}
	}
	if !foundServe {
		t.Fatal("internal WebDAV serving entry point disappeared")
	}
}
