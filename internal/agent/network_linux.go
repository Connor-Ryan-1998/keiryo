//go:build linux

package agent

import (
	"context"
	netstat "github.com/shirou/gopsutil/v4/net"
	"os"
	"path/filepath"
)

func networkCounters(ctx context.Context) ([]netstat.IOCountersStat, error) {
	if root := os.Getenv("HOST_ROOT"); root != "" {
		// /proc/net follows the caller's namespace; PID 1 is the host with --pid=host.
		return netstat.IOCountersByFileWithContext(ctx, true, filepath.Join(root, "proc/1/net/dev"))
	}
	return netstat.IOCountersWithContext(ctx, true)
}
