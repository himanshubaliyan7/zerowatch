//go:build !linux

package procfs

import "errors"

// StatFS is only implemented on Linux.
func StatFS(path string) (FSUsage, error) {
	return FSUsage{}, errors.New("procfs: statfs is only supported on linux")
}
