//go:build linux

package hash

import (
	"os"
	"syscall"
)

// fileIdentity returns the inode-change time (ctime, unix nanos) and inode
// number for fi. See the darwin variant / ADR-0014 for why ctime + inode
// harden the persistent cache.
func fileIdentity(fi os.FileInfo) (ctimeNanos int64, inode uint64, ok bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, false
	}
	return st.Ctim.Nano(), st.Ino, true
}
