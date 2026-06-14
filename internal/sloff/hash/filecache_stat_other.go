//go:build !darwin && !linux

package hash

import "os"

// fileIdentity has no portable ctime/inode source on this platform, so it
// reports ok=false and callers degrade the persistent cache to (size, mtime)
// — see ADR-0014. (Within-run caching is unaffected.)
func fileIdentity(_ os.FileInfo) (ctimeNanos int64, inode uint64, ok bool) {
	return 0, 0, false
}
