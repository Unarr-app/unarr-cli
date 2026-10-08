package cmd

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

type darwinMountPath struct {
	path string
	info os.FileInfo
}

type darwinMountOwner struct {
	mu        sync.Mutex
	directory string
	device    string
	paths     []darwinMountPath
	fsid      *unix.Fsid
	err       error
	list      func() ([]unix.Statfs_t, error)
	unmount   func(string) error
}

// rclone's cgofuse signal path skips host.Unmount. On macOS, explicitly detach
// our kernel volume before signalling the child, including during blocked IO.
func configureRcloneMount(ctx context.Context, cmd *exec.Cmd, directory string) (func() error, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	g, err := newDarwinMountOwner(directory)
	if err != nil {
		return nil, err
	}
	cmd.Args[3] = g.directory
	cmd.Args = append(cmd.Args, "--devname", g.device)
	// launchd stops the daemon's process group. Keep its signal from reaching
	// cgofuse before the daemon can perform the owned kernel detach above.
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(20 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				g.mu.Lock()
				if g.err == nil {
					_, g.err = g.ownedMount()
				}
				g.mu.Unlock()
			}
		}
	}()
	cmd.Cancel = func() error {
		g.mu.Lock()
		g.err = errors.Join(g.err, g.detach())
		err := g.err
		g.mu.Unlock()
		return errors.Join(err, interruptMountProcess(cmd.Process))
	}
	cmd.WaitDelay = 5 * time.Second
	return func() error {
		close(stop)
		<-done
		g.mu.Lock()
		defer g.mu.Unlock()
		g.err = errors.Join(g.err, g.detach())
		return g.err
	}, nil
}

func newDarwinMountOwner(directory string) (*darwinMountOwner, error) {
	dir, err := filepath.EvalSymlinks(directory)
	if err != nil {
		return nil, err
	}
	dir, err = filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return nil, err
	}
	g := &darwinMountOwner{directory: dir, device: "unarr-" + hex.EncodeToString(nonce[:]), list: darwinMountTable, unmount: darwinUnmount}
	for p := dir; ; p = filepath.Dir(p) {
		info, err := os.Lstat(p)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("mount ancestry unavailable: %s: %w", p, err)
		}
		g.paths = append(g.paths, darwinMountPath{p, info})
		if p == filepath.Dir(p) {
			break
		}
	}
	if mount, err := g.ownedMount(); err != nil || mount != nil {
		return nil, fmt.Errorf("mount destination already occupied: %w", err)
	}
	return g, nil
}

func (g *darwinMountOwner) unchangedPaths(mounted bool) error {
	paths := g.paths
	if mounted {
		paths = paths[1:] // The mounted root intentionally hides the original inode.
	}
	for _, p := range paths {
		info, err := os.Lstat(p.path)
		if err != nil || info.Mode()&os.ModeSymlink != 0 || !os.SameFile(info, p.info) {
			return fmt.Errorf("refusing replaced mount path: %s", p.path)
		}
	}
	return nil
}

func (g *darwinMountOwner) ownedMount() (*unix.Statfs_t, error) {
	// Mount appearance/detach can change the leaf inode between a nonblocking
	// table snapshot and Lstat. Re-observe before treating it as a replacement.
	for range 5 {
		before, err := g.sampleMount()
		if err != nil {
			return nil, err
		}
		pathErr := g.unchangedPaths(before != nil)
		after, err := g.sampleMount()
		if err != nil {
			return nil, err
		}
		if !sameDarwinMount(before, after) {
			continue
		}
		if pathErr != nil {
			return nil, pathErr
		}
		if after != nil && g.fsid == nil {
			id := after.Fsid
			g.fsid = &id
		}
		return after, nil
	}
	return nil, errors.New("mount identity changed repeatedly during ownership check")
}

func sameDarwinMount(a, b *unix.Statfs_t) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Fsid == b.Fsid
}

func (g *darwinMountOwner) sampleMount() (*unix.Statfs_t, error) {
	mounts, err := g.list()
	if err != nil {
		return nil, err
	}
	var found *unix.Statfs_t
	for i := range mounts {
		m := &mounts[i]
		if unix.ByteSliceToString(m.Mntonname[:]) != g.directory {
			continue
		}
		if found != nil || !g.matchesMount(m) {
			return nil, errors.New("refusing foreign mount at destination")
		}
		if g.fsid != nil && *g.fsid != m.Fsid {
			return nil, errors.New("refusing replaced mount filesystem ID")
		}
		found = m
	}
	return found, nil
}

func (g *darwinMountOwner) matchesMount(m *unix.Statfs_t) bool {
	return unix.ByteSliceToString(m.Fstypename[:]) == "macfuse" &&
		unix.ByteSliceToString(m.Mntfromname[:]) == g.device &&
		m.Owner == uint32(os.Getuid()) && m.Flags&unix.MNT_RDONLY != 0
}

func (g *darwinMountOwner) detach() error {
	if g.err != nil {
		return g.err // No helper is allowed after an ownership check failed.
	}
	m, err := g.ownedMount()
	if err != nil || m == nil {
		return err
	}
	if err := g.unmount(g.directory); err != nil {
		return err
	}
	m, err = g.ownedMount()
	if err != nil {
		return err
	}
	if m != nil {
		return errors.New("owned kernel mount survived unmount")
	}
	return nil
}

func darwinMountTable() ([]unix.Statfs_t, error) {
	for range 3 {
		n, err := unix.Getfsstat(nil, unix.MNT_NOWAIT)
		if err != nil {
			return nil, err
		}
		buf := make([]unix.Statfs_t, n+16)
		n, err = unix.Getfsstat(buf, unix.MNT_NOWAIT)
		if err != nil {
			return nil, err
		}
		if n < len(buf) {
			return buf[:n], nil
		}
	}
	return nil, errors.New("kernel mount table changed repeatedly")
}

func darwinUnmount(directory string) error {
	// No PATH lookup, shell, privilege elevation, or driver operation.
	return waitDarwinUnmount(exec.Command("/sbin/umount", "-f", directory), 2*time.Second)
}

func waitDarwinUnmount(cmd *exec.Cmd, timeout time.Duration) error {
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("owned macFUSE unmount start: %w", err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case err := <-done:
		if err != nil {
			return fmt.Errorf("owned macFUSE unmount: %w: %s", err, out.String())
		}
		return nil
	case <-timer.C:
		// A kernel-blocked process may survive Kill. Return a truthful failure
		// within the deadline; the waiter retains/reaps this exact helper.
		killErr := cmd.Process.Kill()
		return fmt.Errorf("owned macFUSE unmount PID%d exceeded %s; cleanup incomplete: %w", cmd.Process.Pid, timeout, errors.Join(context.DeadlineExceeded, killErr))
	}
}
