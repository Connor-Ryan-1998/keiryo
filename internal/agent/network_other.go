//go:build !linux

package agent

import (
	"context"
	netstat "github.com/shirou/gopsutil/v4/net"
)

func networkCounters(ctx context.Context) ([]netstat.IOCountersStat, error) {
	return netstat.IOCountersWithContext(ctx, true)
}
