//go:build windows

package sysinfo

import (
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// golang.org/x/sys/windows wraps WTSEnumerateSessions and WTSFreeMemory but not
// WTSQuerySessionInformationW, so it is bound here.
var procWTSQuerySessionInformationW = windows.NewLazySystemDLL("wtsapi32.dll").
	NewProc("WTSQuerySessionInformationW")

const (
	wtsCurrentServerHandle = 0          // WTS_CURRENT_SERVER_HANDLE
	wtsCurrentSession      = 0xFFFFFFFF // WTS_CURRENT_SESSION
	wtsSessionInfoClass    = 24         // WTS_INFO_CLASS WTSSessionInfo → WTSINFOW
)

// wtsInfoW mirrors WTSINFOW. The name arrays are WINSTATIONNAME_LENGTH (32),
// DOMAIN_LENGTH + 1 (17) and USERNAME_LENGTH + 1 (21) WCHARs; Go aligns the
// LARGE_INTEGERs that follow to 8 bytes exactly as the C compiler does, so the
// offsets agree (LogonTime at 200, 216 bytes in all).
type wtsInfoW struct {
	State                   uint32
	SessionID               uint32
	IncomingBytes           uint32
	OutgoingBytes           uint32
	IncomingFrames          uint32
	OutgoingFrames          uint32
	IncomingCompressedBytes uint32
	OutgoingCompressedBytes uint32
	WinStationName          [32]uint16
	Domain                  [17]uint16
	UserName                [21]uint16
	ConnectTime             int64
	DisconnectTime          int64
	LastInputTime           int64
	LogonTime               int64
	CurrentTime             int64
}

func platformSessionLogonTime() (time.Time, bool) {
	var info *wtsInfoW
	var size uint32
	ok, _, _ := procWTSQuerySessionInformationW.Call(
		wtsCurrentServerHandle, wtsCurrentSession, wtsSessionInfoClass,
		uintptr(unsafe.Pointer(&info)), uintptr(unsafe.Pointer(&size)))
	if ok == 0 || info == nil {
		return time.Time{}, false // wtsapi32 missing, or no session info for this token
	}
	defer windows.WTSFreeMemory(uintptr(unsafe.Pointer(info)))
	if size < uint32(unsafe.Sizeof(*info)) {
		return time.Time{}, false // a layout this binding does not know
	}
	// Session 0 (services) and a never-signed-in session report zero. That is
	// not a sign-in at the start of the 17th century; refuse to rule on it.
	if info.LogonTime <= 0 {
		return time.Time{}, false
	}
	ft := windows.Filetime{
		LowDateTime:  uint32(info.LogonTime),
		HighDateTime: uint32(info.LogonTime >> 32),
	}
	return time.Unix(0, ft.Nanoseconds()).UTC(), true
}
