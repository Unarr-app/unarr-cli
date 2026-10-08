package cmd

import (
	"context"
	"fmt"
	"time"
)

func (s *remoteLibrary) revokeAccess(err error) {
	select {
	case s.errors <- fmt.Errorf("mount access: %w", err):
	default:
	}
	s.cancel()
	_ = s.server.Close()
}

// Fail closed on revocation or loss of access verification. A fresh mount must
// pass the server check again; cached catalogs/NNTP credentials grant no lease.
func watchMountAccess(ctx context.Context, check func(context.Context) error, interval time.Duration, stop func(error)) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			probe, cancel := context.WithTimeout(ctx, 10*time.Second)
			err := check(probe)
			cancel()
			if err != nil {
				if ctx.Err() == nil {
					stop(err)
				}
				return
			}
		}
	}
}
