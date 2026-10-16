package procfs

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// Host captures the environment facts the supervisor reasons about.
type Host struct {
	Hostname      string `json:"hostname"`
	OS            string `json:"os"`
	Arch          string `json:"arch"`
	Kernel        string `json:"kernel,omitempty"`
	CPUs          int    `json:"cpus"`
	BTF           bool   `json:"btf"`
	EBPFCapable   bool   `json:"ebpfCapable"`
	InContainer   bool   `json:"inContainer"`
	InKubernetes  bool   `json:"inKubernetes"`
	Cloud         string `json:"cloud,omitempty"`
	DockerSocket  string `json:"dockerSocket,omitempty"`
	Containerd    string `json:"containerdSocket,omitempty"`
	SystemdActive bool   `json:"systemd"`
}

// DetectHost inspects the machine. sysRoot is normally "/" (tests override).
func DetectHost(procRoot, sysRoot string) Host {
	h := Host{OS: runtime.GOOS, Arch: runtime.GOARCH, CPUs: runtime.NumCPU()}
	h.Hostname, _ = os.Hostname()
	if b, err := os.ReadFile(filepath.Join(procRoot, "sys/kernel/osrelease")); err == nil {
		h.Kernel = strings.TrimSpace(string(b))
	}
	_, err := os.Stat(filepath.Join(sysRoot, "sys/kernel/btf/vmlinux"))
	h.BTF = err == nil
	h.EBPFCapable = runtime.GOOS == "linux" && kernelAtLeast(h.Kernel, 5, 8) && h.BTF
	if _, err := os.Stat(filepath.Join(sysRoot, ".dockerenv")); err == nil {
		h.InContainer = true
	}
	if b, err := os.ReadFile(filepath.Join(procRoot, "1/cgroup")); err == nil {
		s := string(b)
		if strings.Contains(s, "docker") || strings.Contains(s, "kubepods") || strings.Contains(s, "containerd") {
			h.InContainer = true
		}
		if strings.Contains(s, "kubepods") {
			h.InKubernetes = true
		}
	}
	if os.Getenv("KUBERNETES_SERVICE_HOST") != "" {
		h.InKubernetes, h.InContainer = true, true
	}
	if b, err := os.ReadFile(filepath.Join(sysRoot, "sys/class/dmi/id/sys_vendor")); err == nil {
		v := strings.ToLower(string(b))
		switch {
		case strings.Contains(v, "amazon"):
			h.Cloud = "aws"
		case strings.Contains(v, "google"):
			h.Cloud = "gcp"
		case strings.Contains(v, "microsoft"):
			h.Cloud = "azure"
		}
	}
	for _, p := range []string{"var/run/docker.sock", "run/docker.sock"} {
		if fi, err := os.Stat(filepath.Join(sysRoot, p)); err == nil && fi.Mode()&os.ModeSocket != 0 {
			h.DockerSocket = filepath.Join(sysRoot, p)
			break
		}
	}
	if fi, err := os.Stat(filepath.Join(sysRoot, "run/containerd/containerd.sock")); err == nil && fi.Mode()&os.ModeSocket != 0 {
		h.Containerd = filepath.Join(sysRoot, "run/containerd/containerd.sock")
	}
	if fi, err := os.Stat(filepath.Join(sysRoot, "run/systemd/system")); err == nil && fi.IsDir() {
		h.SystemdActive = true
	}
	return h
}

func kernelAtLeast(rel string, major, minor int) bool {
	var ma, mi int
	if _, err := fmt.Sscanf(rel, "%d.%d", &ma, &mi); err != nil {
		return false
	}
	return ma > major || (ma == major && mi >= minor)
}

// Container is a summary from the Docker Engine API.
type Container struct {
	ID     string            `json:"id"`
	Name   string            `json:"name"`
	Image  string            `json:"image"`
	State  string            `json:"state"`
	Labels map[string]string `json:"labels,omitempty"`
	Ports  []uint16          `json:"ports,omitempty"`
}

// ListDockerContainers calls GET /containers/json over the unix socket.
func ListDockerContainers(ctx context.Context, socket string) ([]Container, error) {
	if socket == "" {
		return nil, nil
	}
	c := &http.Client{
		Timeout: 2 * time.Second,
		Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socket)
		}},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://docker/containers/json", nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.Do(req)
	if err != nil {
		return nil, fmt.Errorf("docker socket: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("docker socket: status %d", resp.StatusCode)
	}
	var raw []struct {
		ID     string            `json:"Id"`
		Names  []string          `json:"Names"`
		Image  string            `json:"Image"`
		State  string            `json:"State"`
		Labels map[string]string `json:"Labels"`
		Ports  []struct {
			PrivatePort uint16 `json:"PrivatePort"`
		} `json:"Ports"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&raw); err != nil {
		return nil, fmt.Errorf("docker socket: decode: %w", err)
	}
	out := make([]Container, 0, len(raw))
	for _, r := range raw {
		c := Container{ID: r.ID[:min(12, len(r.ID))], Image: r.Image, State: r.State, Labels: r.Labels}
		if len(r.Names) > 0 {
			c.Name = strings.TrimPrefix(r.Names[0], "/")
		}
		for _, p := range r.Ports {
			c.Ports = append(c.Ports, p.PrivatePort)
		}
		out = append(out, c)
	}
	return out, nil
}
