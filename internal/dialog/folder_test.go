package dialog

import (
	"errors"
	"os/exec"
	"runtime"
	"strings"
	"testing"
)

func stubFolderPickers(t *testing.T, cmds ...*exec.Cmd) {
	t.Helper()
	old := folderCandidates
	folderCandidates = func(string, string) []*exec.Cmd { return cmds }
	t.Cleanup(func() { folderCandidates = old })
}

func TestPickFolderReturnsTheChosenPath(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses a POSIX shell as the stand-in picker")
	}
	stubFolderPickers(t, exec.Command("sh", "-c", "echo /srv/media"))
	got, err := PickFolder("t", "")
	if err != nil || got != "/srv/media" {
		t.Fatalf("got %q, %v; want /srv/media, nil", got, err)
	}
}

func TestPickFolderMissingProgramFallsThroughNotCancelled(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses a POSIX shell as the stand-in picker")
	}
	// A picker that is not installed must not read as the user cancelling.
	stubFolderPickers(t, exec.Command("definitely-not-a-picker"), exec.Command("sh", "-c", "echo /ok"))
	if got, err := PickFolder("t", ""); err != nil || got != "/ok" {
		t.Fatalf("got %q, %v; want the second candidate's answer", got, err)
	}
	stubFolderPickers(t, exec.Command("definitely-not-a-picker"))
	if _, err := PickFolder("t", ""); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err = %v, want ErrUnavailable", err)
	}
}

func TestPickFolderNonZeroExitOrEmptyOutputIsCancel(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses a POSIX shell as the stand-in picker")
	}
	for _, script := range []string{"exit 1", "true"} {
		stubFolderPickers(t, exec.Command("sh", "-c", script))
		if _, err := PickFolder("t", ""); !errors.Is(err, ErrCancelled) {
			t.Errorf("%q: err = %v, want ErrCancelled", script, err)
		}
	}
}

func TestFolderCandidatesCarryTitleAndInitial(t *testing.T) {
	dir := t.TempDir()
	for _, c := range folderCandidates("Pick it", dir) {
		joined := strings.Join(c.Args, " ")
		if !strings.Contains(joined, "Pick it") {
			t.Errorf("%s lost the title: %v", c.Path, c.Args)
		}
		if !strings.Contains(joined, dir) {
			t.Errorf("%s lost the starting folder: %v", c.Path, c.Args)
		}
	}
}
