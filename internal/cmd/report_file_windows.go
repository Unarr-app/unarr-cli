package cmd

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

// The protected DACL is present at creation: tightening an inherited ACL later
// would leave a window for another user to open and retain a readable handle.
func createPrivateReportFile() (*os.File, error) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return nil, err
	}
	sd, err := windows.SecurityDescriptorFromString("D:P(A;;FA;;;" + user.User.Sid.String() + ")")
	if err != nil {
		return nil, err
	}
	sa := windows.SecurityAttributes{SecurityDescriptor: sd}
	sa.Length = uint32(unsafe.Sizeof(sa))
	for range 10 {
		var nonce [16]byte
		if _, err := rand.Read(nonce[:]); err != nil {
			return nil, err
		}
		name := "unarr-report-" + hex.EncodeToString(nonce[:]) + ".json"
		path, err := windows.UTF16PtrFromString(name)
		if err != nil {
			return nil, err
		}
		// CREATE_NEW refuses existing files and symlinks; no sharing prevents
		// another process opening the report before writing and closing finish.
		h, err := windows.CreateFile(path, windows.GENERIC_READ|windows.GENERIC_WRITE, 0, &sa, windows.CREATE_NEW, windows.FILE_ATTRIBUTE_NORMAL, 0)
		if errors.Is(err, windows.ERROR_FILE_EXISTS) || errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
			continue
		}
		if err != nil {
			return nil, err
		}
		f := os.NewFile(uintptr(h), name)
		// Filesystems that cannot enforce ACLs must fail before any payload is
		// written. Query the same handle, never a replaceable pathname.
		if err := verifyPrivateReportACL(h, user.User.Sid); err != nil {
			_ = f.Close()
			_ = os.Remove(name)
			return nil, err
		}
		return f, nil
	}
	return nil, errors.New("cannot create unique report file")
}

func verifyPrivateReportACL(h windows.Handle, sid *windows.SID) error {
	sd, err := windows.GetSecurityInfo(h, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return err
	}
	control, _, err := sd.Control()
	if err != nil {
		return err
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return err
	}
	if control&windows.SE_DACL_PROTECTED == 0 || dacl == nil || dacl.AceCount != 1 {
		return errors.New("report filesystem does not enforce a private ACL")
	}
	var ace *windows.ACCESS_ALLOWED_ACE
	if err := windows.GetAce(dacl, 0, &ace); err != nil {
		return err
	}
	// GetAce returns an OS-validated ACE from GetSecurityInfo. SidStart is the
	// inline variable-length SID, not a pointer field; this is its Win32 layout.
	// The descriptor remains live through this comparison.
	if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE || !(*windows.SID)(unsafe.Pointer(&ace.SidStart)).Equals(sid) { // #nosec G103 -- audited Win32 inline SID layout
		return errors.New("report ACL grants access to another identity")
	}
	return nil
}
