package mountsetup

import (
	"runtime"
	"strings"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Parse the descriptor actually received by the child through Windows APIs;
// checking just the literal would miss permissive, inherited or owner grants.
func TestMountFlagsWindowsProtectedOwnerReadExecute(t *testing.T) {
	var descriptors []string
	for _, arg := range capabilityProbeArguments(t) {
		if value, ok := strings.CutPrefix(arg, "FileSecurity="); ok {
			descriptors = append(descriptors, value)
		}
	}
	if len(descriptors) != 1 {
		t.Fatal("expected one actual filesystem security descriptor", descriptors)
	}
	sd, err := windows.SecurityDescriptorFromString(descriptors[0])
	if err != nil {
		t.Fatal("Windows rejected the emitted descriptor", err)
	}
	control, _, err := sd.Control()
	if err != nil || control&windows.SE_DACL_PROTECTED == 0 {
		t.Fatal("descriptor allows inherited permissions", control, err)
	}
	dacl, defaulted, err := sd.DACL()
	if err != nil || dacl == nil || defaulted || dacl.AceCount != 1 {
		t.Fatal("descriptor has a missing, permissive or extra-grant DACL", dacl, defaulted, err)
	}
	var ace *windows.ACCESS_ALLOWED_ACE
	if err := windows.GetAce(dacl, 0, &ace); err != nil || ace == nil {
		t.Fatal("inspect emitted owner rights", err)
	}
	wantRights := windows.ACCESS_MASK(windows.FILE_GENERIC_READ | windows.FILE_GENERIC_EXECUTE)
	if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE || ace.Header.AceFlags != 0 || ace.Mask != wantRights {
		t.Fatalf("owner grant type=%d flags=0x%x rights=0x%x want read/execute=0x%x", ace.Header.AceType, ace.Header.AceFlags, ace.Mask, wantRights)
	}
	sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
	if got := sid.String(); got != "S-1-3-4" { // OWNER RIGHTS, rather than a particular user SID.
		t.Fatal("descriptor does not restrict OWNER RIGHTS", got)
	}
	t.Logf("actual child descriptor: protected=true owner-rights=%s mask=0x%x", sid.String(), ace.Mask)
	runtime.KeepAlive(sd)
}
