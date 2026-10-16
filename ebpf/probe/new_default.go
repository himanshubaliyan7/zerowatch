//go:build !(linux && ebpf)

package probe

import "log/slog"

// Available reports whether this binary was built with the eBPF loader.
const Available = false

// New returns the best probe compiled into this binary. Default builds ship
// only the /proc fallback; build with `-tags ebpf` on Linux for kernel probes.
// objPath is ignored here.
func New(log *slog.Logger, objPath string) Probe {
	_ = objPath
	return NewProcProbe(log)
}
