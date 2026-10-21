package orchestrator

import (
	"context"
	"errors"
	"io/fs"
	"path/filepath"
	"strings"
	"time"
)

// detailLogTail is the number of recent log lines shown in the instance detail
// page (spec F-06 / M4).
const detailLogTail = 200

type statsEntry struct {
	stats ResourceStats
	at    time.Time
}

// InstanceDetail is everything the detail page needs in one round trip: the
// instance plus live container state, recently masked logs, a resource snapshot
// and the size of its data directory.
type InstanceDetail struct {
	InstanceView

	Logs           string        `json:"logs"`
	LogsLines      int           `json:"logs_lines"`
	LogsAvailable  bool          `json:"logs_available"`
	DataDir        string        `json:"data_dir"`
	DataSizeBytes  int64         `json:"data_size_bytes"`
	Stats          ResourceStats `json:"stats"`
	TokenAvailable bool          `json:"token_available"`
}

// Detail returns the aggregated detail view. Failures to read logs, stats or
// the data directory are reported as unavailable instead of failing the whole
// page: the instance itself is still manageable and the UI must not white-screen
// just because Docker is temporarily unreachable.
func (m *Manager) Detail(ctx context.Context, id string) (InstanceDetail, error) {
	inst, err := m.instance(id)
	if err != nil {
		return InstanceDetail{}, err
	}
	// Even a read-only detail request must not touch a container that carries
	// someone else's name without the ts3panel labels (iron rule 5). A Docker
	// outage is tolerated so the page still shows the stored state.
	if _, inspectErr := m.inspectManaged(ctx, &inst); inspectErr != nil && errors.Is(inspectErr, ErrNotManaged) {
		return InstanceDetail{}, inspectErr
	}

	detail := InstanceDetail{
		InstanceView:   m.view(ctx, inst),
		DataDir:        m.instanceDir(inst.ID),
		TokenAvailable: strings.TrimSpace(inst.AdminToken) != "",
	}

	logs, logErr := m.runtime.Logs(ctx, inst.Container, detailLogTail)
	if logErr != nil {
		// Log only the reason, never the log body (iron rule 9).
		m.logger.Warn("read instance logs failed", "instance_id", inst.ID, "err", logErr)
	} else {
		masked := MaskSecrets(logs)
		detail.Logs = masked
		detail.LogsLines = countLines(masked)
		detail.LogsAvailable = true
	}

	detail.DataSizeBytes = dirSize(ctx, detail.DataDir)
	detail.Stats = m.resourceStats(ctx, inst.Container)
	return detail, nil
}

func countLines(text string) int {
	trimmed := strings.TrimRight(text, "\n")
	if trimmed == "" {
		return 0
	}
	return strings.Count(trimmed, "\n") + 1
}

// forgetStats drops a cached snapshot after a lifecycle change so the detail
// page does not show CPU/memory from before a stop or restart.
func (m *Manager) forgetStats(name string) {
	m.statsMu.Lock()
	delete(m.statsCache, name)
	m.statsMu.Unlock()
}

// resourceStats returns a cached snapshot. A CPU percentage needs two readings,
// so a cache miss costs one StatsSampleGap (default 1s); all other detail
// requests within StatsTTL (default 10s) are served from memory. The mutex is
// held across the sampling so a burst of detail requests does not multiply
// Docker calls.
func (m *Manager) resourceStats(ctx context.Context, name string) ResourceStats {
	m.statsMu.Lock()
	defer m.statsMu.Unlock()

	now := m.now()
	if cached, ok := m.statsCache[name]; ok && now.Sub(cached.at) < m.statsTTL {
		return cached.stats
	}

	unavailable := ResourceStats{Available: false, SampledAt: now.UTC()}
	first, err := m.runtime.Stats(ctx, name)
	if err != nil {
		m.logger.Warn("read container stats failed", "container", name, "err", err)
		return unavailable
	}

	select {
	case <-ctx.Done():
		return unavailable
	case <-time.After(m.statsGap):
	}

	second, err := m.runtime.Stats(ctx, name)
	if err != nil {
		m.logger.Warn("read container stats failed", "container", name, "err", err)
		return unavailable
	}

	stats := computeResourceStats(first, second)
	stats.SampledAt = m.now().UTC()
	m.statsCache[name] = statsEntry{stats: stats, at: m.now()}
	return stats
}

// dirSize sums regular files under root, skipping unreadable entries. The panel
// must never fail a page because one TS3 file is owned by another uid.
func dirSize(ctx context.Context, root string) int64 {
	var total int64
	visited := 0
	_ = filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			if entry != nil && entry.IsDir() && path != root {
				return fs.SkipDir
			}
			return nil
		}
		visited++
		if visited%256 == 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			default:
			}
		}
		if entry.IsDir() {
			return nil
		}
		info, infoErr := entry.Info()
		if infoErr != nil || !info.Mode().IsRegular() {
			return nil
		}
		total += info.Size()
		return nil
	})
	return total
}
