package cmd

import (
	"runtime"
	"strings"
	"testing"
)

func TestValidateDownloadDirRejectsSystemDirInsideTheForm(t *testing.T) {
	bad := "/etc"
	if runtime.GOOS == "windows" {
		bad = `D:\`
	}
	err := validateDownloadDir(bad)
	if err == nil {
		t.Fatalf("%q accepted, want rejection", bad)
	}
	if runtime.GOOS == "windows" && !strings.Contains(err.Error(), `D:\Downloads`) {
		t.Errorf("drive-root error should suggest a folder, got: %v", err)
	}
}

func TestValidateDownloadDirAcceptsAFolderAndEmpty(t *testing.T) {
	for _, ok := range []string{"", t.TempDir()} {
		if err := validateDownloadDir(ok); err != nil {
			t.Errorf("%q rejected: %v", ok, err)
		}
	}
}
