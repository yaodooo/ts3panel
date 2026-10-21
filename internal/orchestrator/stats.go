package orchestrator

import (
	"context"
	"encoding/json"
	"io"
	"math"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/errdefs"
)

// StatsSample is one raw cgroup counter reading. CPU time is a monotonic
// counter, so a useful CPU percentage needs two readings with a small gap.
// Memory is a gauge and is valid on its own.
type StatsSample struct {
	TotalCPU    uint64
	SystemCPU   uint64
	OnlineCPUs  int
	MemoryUsed  uint64
	MemoryLimit uint64
	ReadAt      time.Time
}

// ResourceStats is the panel-facing resource snapshot. Available is false when
// Docker could not be queried (container missing, daemon down); the UI must show
// "unavailable" instead of a fake zero. CPUAvailable is false when two readings
// could not produce a delta (just started, container restarted in between).
type ResourceStats struct {
	CPUPercent       float64   `json:"cpu_percent"`
	CPUAvailable     bool      `json:"cpu_available"`
	MemoryBytes      int64     `json:"memory_bytes"`
	MemoryLimitBytes int64     `json:"memory_limit_bytes"`
	SampledAt        time.Time `json:"sampled_at"`
	Available        bool      `json:"available"`
}

// computeResourceStats turns two cumulative readings into a snapshot. Docker's
// own percentage formula applies the number of online CPUs:
//
//	(cpuDelta / systemDelta) * onlineCPUs * 100
//
// Counter resets (container restarted between reads) leave CPU unavailable
// rather than reporting a negative or absurd number.
func computeResourceStats(first, second StatsSample) ResourceStats {
	out := ResourceStats{
		MemoryBytes:      int64(second.MemoryUsed),
		MemoryLimitBytes: int64(second.MemoryLimit),
		SampledAt:        second.ReadAt.UTC(),
		Available:        !second.ReadAt.IsZero(),
	}
	if out.SampledAt.IsZero() {
		out.SampledAt = time.Now().UTC()
	}
	if second.SystemCPU > first.SystemCPU && second.TotalCPU >= first.TotalCPU {
		cpus := second.OnlineCPUs
		if cpus <= 0 {
			cpus = 1
		}
		cpuDelta := float64(second.TotalCPU - first.TotalCPU)
		systemDelta := float64(second.SystemCPU - first.SystemCPU)
		percent := cpuDelta / systemDelta * float64(cpus) * 100
		if percent < 0 {
			percent = 0
		}
		out.CPUPercent = math.Round(percent*100) / 100
		out.CPUAvailable = true
	}
	return out
}

// Stats reads one Docker stats sample. It is the production implementation of
// Runtime.Stats.
func (d *dockerRuntime) Stats(ctx context.Context, name string) (StatsSample, error) {
	resp, err := d.cli.ContainerStatsOneShot(ctx, name)
	if err != nil {
		if errdefs.IsNotFound(err) {
			return StatsSample{}, ErrContainerMissing
		}
		return StatsSample{}, err
	}
	defer resp.Body.Close()

	var payload container.StatsResponse
	// A stats frame is normally a few kilobytes; the cap only protects against a
	// malformed daemon response.
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&payload); err != nil {
		return StatsSample{}, err
	}

	used := payload.MemoryStats.Usage
	// Match `docker stats`: report working-set memory by subtracting the page
	// cache. cgroup v1 exposes total_inactive_file, cgroup v2 inactive_file.
	for _, key := range []string{"total_inactive_file", "inactive_file"} {
		if cache, ok := payload.MemoryStats.Stats[key]; ok && cache <= used {
			used -= cache
			break
		}
	}
	online := int(payload.CPUStats.OnlineCPUs)
	if online <= 0 {
		online = len(payload.CPUStats.CPUUsage.PercpuUsage)
	}
	return StatsSample{
		TotalCPU:    payload.CPUStats.CPUUsage.TotalUsage,
		SystemCPU:   payload.CPUStats.SystemUsage,
		OnlineCPUs:  online,
		MemoryUsed:  used,
		MemoryLimit: payload.MemoryStats.Limit,
		ReadAt:      time.Now().UTC(),
	}, nil
}
