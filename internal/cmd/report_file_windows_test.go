package cmd

import (
	"os"
	"testing"

	"golang.org/x/sys/windows"
)

func TestReportFilePrivateACLFromCreation(t *testing.T) {
	t.Chdir(t.TempDir())
	f, err := createPrivateReportFile()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	if err := verifyPrivateReportACL(windows.Handle(f.Fd()), user.User.Sid); err != nil {
		t.Fatal(err)
	}
	if info, err := f.Stat(); err != nil || info.Size() != 0 {
		t.Fatalf("protection happened after writing: %v %v", info, err)
	}
	if other, err := os.Open(f.Name()); err == nil {
		_ = other.Close()
		t.Fatal("report was shareable before completing its write")
	}
	if _, err := f.WriteString("private report"); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(f.Name())
	if err != nil || string(got) != "private report" {
		t.Fatalf("owner cannot read report: %q %v", got, err)
	}
}
