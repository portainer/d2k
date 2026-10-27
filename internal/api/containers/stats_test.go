package containers

import (
	"math"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
)

// cpuPercent is the Docker CLI's calculation (cli/command/container/stats_helpers.go).
func cpuPercent(s container.StatsResponse) float64 {
	cpuDelta := float64(s.CPUStats.CPUUsage.TotalUsage) - float64(s.PreCPUStats.CPUUsage.TotalUsage)
	systemDelta := float64(s.CPUStats.SystemUsage) - float64(s.PreCPUStats.SystemUsage)
	if systemDelta <= 0 || cpuDelta <= 0 {
		return 0
	}
	return cpuDelta / systemDelta * float64(s.CPUStats.OnlineCPUs) * 100
}

func TestCPUClockGivesTheKubernetesRate(t *testing.T) {
	start := time.Now()
	var clock cpuClock
	// First sample: the previous one is placed a second earlier at the same rate.
	s := clock.sample(1_500_000_000, 4, start) // 1.5 cores on a 4-CPU host
	if p := cpuPercent(s); math.Abs(p-150) > 0.01 {
		t.Fatalf("first sample CPU = %.2f%%, want 150%%", p)
	}
	s = clock.sample(250_000_000, 4, start.Add(2*time.Second)) // then a quarter core
	if p := cpuPercent(s); math.Abs(p-25) > 0.01 {
		t.Fatalf("second sample CPU = %.2f%%, want 25%%", p)
	}
	if !s.PreRead.Equal(start) || !s.Read.Equal(start.Add(2*time.Second)) {
		t.Fatalf("read times = %v, %v", s.PreRead, s.Read)
	}
}
