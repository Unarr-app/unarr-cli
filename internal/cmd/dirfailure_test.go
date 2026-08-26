package cmd

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"syscall"
	"testing"

	"github.com/Unarr-app/unarr-cli/internal/agent"
)

func TestDirFailureEvent(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
		why  string
	}{
		{
			name: "the malformed Windows path from the crash report",
			// What os.MkdirAll(`D:\D:\`) hands back on Windows: ERROR_INVALID_NAME,
			// wrapped in *PathError exactly like this.
			err:  &os.PathError{Op: "mkdir", Path: `D:\D:\`, Err: syscall.Errno(123)},
			want: agent.EventConfigError,
			why:  "a path the OS cannot parse is a config problem; reporting it as permission_denied sends the user to Run-as-Administrator for a typo",
		},
		{
			name: "a genuine permission failure",
			err:  &os.PathError{Op: "mkdir", Path: "/srv/media", Err: fs.ErrPermission},
			want: agent.EventPermissionDenied,
			why:  "the one case the old code was right about must keep working",
		},
		{
			name: "a wrapped permission failure",
			err:  fmt.Errorf("create download dir: %w", &os.PathError{Op: "mkdir", Path: "/srv/media", Err: fs.ErrPermission}),
			want: agent.EventPermissionDenied,
			why:  "classification must survive wrapping, since callers add context",
		},
		{
			name: "a missing drive",
			err:  &os.PathError{Op: "mkdir", Path: `E:\Media`, Err: fs.ErrNotExist},
			want: agent.EventConfigError,
			why:  "an unplugged drive is the user's setup to fix, not a permissions denial",
		},
		{
			name: "an unknown error",
			err:  errors.New("disk quota exceeded"),
			want: agent.EventConfigError,
			why:  "the default must be the honest 'something about your setup', not a specific wrong claim",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := dirFailureEvent(tc.err); got != tc.want {
				t.Fatalf("dirFailureEvent(%v) = %q, want %q — %s", tc.err, got, tc.want, tc.why)
			}
		})
	}
}
