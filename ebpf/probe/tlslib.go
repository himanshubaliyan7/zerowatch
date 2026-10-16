package probe

import (
	"bytes"
	"path"
	"regexp"
	"strings"
)

// Helpers for finding a process's OpenSSL library. They only parse /proc
// text, so they live in the default build and are unit tested there; the
// uprobe attacher that uses them is tls_linux_ebpf.go.

// sslLibRe matches OpenSSL's libssl by file name: libssl.so, libssl.so.3,
// libssl.so.1.1. It does not match libssl3.so (NSS) or libssl-*.so builds.
var sslLibRe = regexp.MustCompile(`^libssl\.so(\.[0-9]+)*$`)

// findSSLLibs returns the distinct libssl paths in a /proc/<pid>/maps
// listing, in order of first appearance. Paths are as the target sees them
// (its own mount namespace). Deleted files are skipped: they cannot be opened
// by path any more, so a uprobe cannot be attached to them.
func findSSLLibs(maps []byte) []string {
	var out []string
	seen := map[string]bool{}
	for _, line := range strings.Split(string(maps), "\n") {
		// address perms offset dev inode pathname
		f := strings.Fields(line)
		if len(f) < 6 || !strings.HasPrefix(f[5], "/") {
			continue
		}
		p := strings.Join(f[5:], " ")
		if strings.HasSuffix(p, " (deleted)") {
			continue
		}
		if !sslLibRe.MatchString(path.Base(p)) || seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	return out
}

// procStartTime returns field 22 (starttime, in clock ticks since boot) of a
// /proc/<pid>/stat line, or "" if it cannot be parsed. Together with the PID
// it identifies a process across PID reuse.
func procStartTime(stat []byte) string {
	// comm (field 2) is in parentheses and may contain spaces or ')'.
	i := bytes.LastIndexByte(stat, ')')
	if i < 0 {
		return ""
	}
	f := strings.Fields(string(stat[i+1:]))
	// f[0] is field 3 (state), so field 22 is f[19].
	if len(f) < 20 {
		return ""
	}
	return f[19]
}
