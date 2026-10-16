//go:build linux

package procfs

import "syscall"

// StatFS returns the capacity of the filesystem mounted at path.
func StatFS(path string) (FSUsage, error) {
	var s syscall.Statfs_t
	if err := syscall.Statfs(path, &s); err != nil {
		return FSUsage{}, err
	}
	bs := uint64(s.Bsize)
	return FSUsage{Total: s.Blocks * bs, Free: s.Bfree * bs, Avail: s.Bavail * bs, Inodes: s.Files, InodesFree: s.Ffree}, nil
}
