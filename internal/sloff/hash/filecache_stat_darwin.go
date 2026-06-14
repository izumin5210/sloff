//go:build darwin

package hash

import (
	"os"
	"syscall"
)

// fileIdentity returns the inode-change time (ctime, unix nanos) and inode
// number for fi. ctime updates on any content or metadata change and cannot be
// preserved by userspace copy tools (rsync --times / tar / cp -p), which is why
// it hardens the persistent (size, mtime) cache against stale hits — see
// ADR-0014. ok is false when the platform stat is unavailable, in which case
// callers degrade to (size, mtime).
func fileIdentity(fi os.FileInfo) (ctimeNanos int64, inode uint64, ok bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, false
	}
	return st.Ctimespec.Nano(), st.Ino, true
}
