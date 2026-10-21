package backup

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"ts3panel/internal/store"
)

// Scheduler runs the automatic backup plan. It is a plain in-process ticker:
// no system cron, no external state. The next run time is persisted in
// state.json, which is what stops a panel restart from running the same backup
// twice and lets a missed run be caught up once.
type Scheduler struct {
	cfg   *store.ConfigStore
	state *store.StateStore
	mgr   *Manager
	now   func() time.Time
	tick  time.Duration
	log   *slog.Logger

	wake chan struct{}
	stop chan struct{}
	done chan struct{}

	startOnce sync.Once
	stopOnce  sync.Once
	runMu     sync.Mutex
	loopMu    sync.Mutex
	looping   bool
}

type SchedulerOptions struct {
	Config  *store.ConfigStore
	State   *store.StateStore
	Manager *Manager
	Now     func() time.Time
	Tick    time.Duration
	Logger  *slog.Logger
}

func NewScheduler(opts SchedulerOptions) (*Scheduler, error) {
	if opts.Config == nil || opts.State == nil || opts.Manager == nil {
		return nil, errors.New("backup: scheduler needs config, state and manager")
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	tick := opts.Tick
	if tick <= 0 {
		tick = 30 * time.Second
	}
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	return &Scheduler{
		cfg:   opts.Config,
		state: opts.State,
		mgr:   opts.Manager,
		now:   now,
		tick:  tick,
		log:   log,
		wake:  make(chan struct{}, 1),
		stop:  make(chan struct{}),
		done:  make(chan struct{}),
	}, nil
}

// Start reconciles the persisted schedule and starts the ticker when the plan is
// enabled. It never fails: a broken state file is logged and retried on the
// next tick, because the panel itself must keep working.
func (s *Scheduler) Start() {
	s.startOnce.Do(func() {
		if err := s.Reconcile(); err != nil {
			s.log.Warn("backup schedule reconcile failed", "err", err)
		}
		if s.cfg.Snapshot().Backup.Enabled {
			s.ensureLoop()
		}
	})
}

// Stop terminates the ticker. Safe to call more than once.
func (s *Scheduler) Stop() {
	s.stopOnce.Do(func() {
		close(s.stop)
		s.loopMu.Lock()
		looping := s.looping
		s.loopMu.Unlock()
		if looping {
			// Do not hang shutdown behind a long backup; the process is exiting
			// and an interrupted run is recovered by the claimed NextRunAt.
			select {
			case <-s.done:
			case <-time.After(5 * time.Second):
			}
		}
	})
}

// Reschedule recomputes the next run after the plan was changed. The caller
// must have saved config.toml first.
func (s *Scheduler) Reschedule() error {
	if err := s.Reconcile(); err != nil {
		return err
	}
	if s.cfg.Snapshot().Backup.Enabled {
		s.ensureLoop()
	}
	return nil
}

func (s *Scheduler) ensureLoop() {
	s.loopMu.Lock()
	defer s.loopMu.Unlock()
	if s.looping {
		return
	}
	s.looping = true
	go s.loop()
}

func (s *Scheduler) loop() {
	defer close(s.done)
	ticker := time.NewTicker(s.tick)
	defer ticker.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-s.wake:
			s.tickOnce()
		case <-ticker.C:
			s.tickOnce()
		}
	}
}

// Reconcile writes the next run time for the current configuration. When the
// plan is disabled the field is cleared so the UI does not show a stale date.
func (s *Scheduler) Reconcile() error {
	cfg := s.cfg.Snapshot().Backup
	st, err := s.state.Snapshot()
	if err != nil {
		return err
	}
	if !cfg.Enabled {
		if st.Backup.NextRunAt == nil {
			return nil
		}
		return s.state.Update(func(cur *store.State) error {
			cur.Backup.NextRunAt = nil
			return nil
		})
	}
	next, err := computeNext(s.now(), cfg, st.Backup.LastSuccessAt)
	if err != nil {
		return err
	}
	if st.Backup.NextRunAt != nil && st.Backup.NextRunAt.Equal(next) {
		return nil
	}
	return s.state.Update(func(cur *store.State) error {
		cur.Backup.NextRunAt = timePtr(next)
		return nil
	})
}

func (s *Scheduler) tickOnce() {
	cfg := s.cfg.Snapshot().Backup
	now := s.now()

	st, err := s.state.Snapshot()
	if err != nil {
		s.log.Warn("backup scheduler state read failed", "err", err)
		return
	}
	if !cfg.Enabled {
		if st.Backup.NextRunAt != nil {
			_ = s.state.Update(func(cur *store.State) error {
				cur.Backup.NextRunAt = nil
				return nil
			})
		}
		return
	}
	if st.Backup.NextRunAt == nil {
		next, err := computeNext(now, cfg, st.Backup.LastSuccessAt)
		if err != nil {
			s.log.Warn("backup schedule calculation failed", "err", err)
			return
		}
		_ = s.state.Update(func(cur *store.State) error {
			cur.Backup.NextRunAt = timePtr(next)
			return nil
		})
		return
	}
	if now.Before(*st.Backup.NextRunAt) {
		return
	}
	s.runDue(cfg, now)
}

// runDue claims the due run before starting it. If the panel dies mid-backup the
// next start sees a future NextRunAt instead of immediately repeating the work.
func (s *Scheduler) runDue(cfg store.BackupConfig, now time.Time) {
	s.runMu.Lock()
	defer s.runMu.Unlock()

	// Re-check under the lock: another tick may have just run.
	st, err := s.state.Snapshot()
	if err != nil || !cfg.Enabled {
		return
	}
	if st.Backup.NextRunAt == nil || now.Before(*st.Backup.NextRunAt) {
		return
	}

	next, err := computeNextAfter(now, cfg)
	if err != nil {
		s.log.Warn("backup schedule calculation failed", "err", err)
		return
	}
	if err := s.state.Update(func(cur *store.State) error {
		cur.Backup.LastAttemptAt = timePtr(now)
		cur.Backup.NextRunAt = timePtr(next)
		return nil
	}); err != nil {
		s.log.Warn("backup schedule claim failed", "err", err)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Minute)
	defer cancel()

	instances := st.Instances
	var failures []string
	okCount := 0
	for _, inst := range instances {
		if _, err := s.mgr.Create(ctx, inst.ID, true); err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", inst.ID, err))
			continue
		}
		okCount++
		if removed, err := s.mgr.Prune(ctx, inst.ID, cfg.Retention); err != nil {
			s.log.Warn("prune backups failed", "instance_id", inst.ID, "err", err)
		} else if removed > 0 {
			s.log.Info("old backups pruned", "instance_id", inst.ID, "removed", removed)
		}
	}

	message := ""
	if len(failures) > 0 {
		message = truncateMessage(strings.Join(failures, "; "), 500)
	}
	if err := s.state.Update(func(cur *store.State) error {
		if len(failures) == 0 {
			cur.Backup.LastSuccessAt = timePtr(now)
			cur.Backup.LastError = ""
		} else {
			cur.Backup.LastError = message
		}
		return nil
	}); err != nil {
		s.log.Error("backup run result not saved", "err", err)
	}
	s.log.Info("automatic backup finished", "instances", len(instances), "ok", okCount, "failed", len(failures), "next", next)
}

// computeNext returns the next run after now, considering the last success.
// With no previous success a daily plan waits for its next occurrence and an
// interval plan waits one full interval; it does not fire immediately on enable.
func computeNext(now time.Time, cfg store.BackupConfig, lastSuccess *time.Time) (time.Time, error) {
	switch cfg.Mode {
	case "interval":
		if cfg.IntervalHours < 1 {
			return time.Time{}, errors.New("interval_hours must be positive")
		}
		base := now
		if lastSuccess != nil {
			base = *lastSuccess
		}
		return base.Add(time.Duration(cfg.IntervalHours) * time.Hour), nil
	case "daily", "":
		hour, minute, err := parseDailyAt(cfg.DailyAt)
		if err != nil {
			return time.Time{}, err
		}
		cand := time.Date(now.Year(), now.Month(), now.Day(), hour, minute, 0, 0, now.Location())
		if lastSuccess == nil {
			if !cand.After(now) {
				cand = cand.AddDate(0, 0, 1)
			}
			return cand, nil
		}
		if !cand.After(*lastSuccess) {
			cand = cand.AddDate(0, 0, 1)
		}
		return cand, nil
	default:
		return time.Time{}, fmt.Errorf("unsupported backup mode %q", cfg.Mode)
	}
}

// computeNextAfter returns the occurrence strictly after now, used right after a
// run so the same slot cannot fire twice.
func computeNextAfter(now time.Time, cfg store.BackupConfig) (time.Time, error) {
	switch cfg.Mode {
	case "interval":
		if cfg.IntervalHours < 1 {
			return time.Time{}, errors.New("interval_hours must be positive")
		}
		return now.Add(time.Duration(cfg.IntervalHours) * time.Hour), nil
	case "daily", "":
		hour, minute, err := parseDailyAt(cfg.DailyAt)
		if err != nil {
			return time.Time{}, err
		}
		cand := time.Date(now.Year(), now.Month(), now.Day(), hour, minute, 0, 0, now.Location())
		if !cand.After(now) {
			cand = cand.AddDate(0, 0, 1)
		}
		return cand, nil
	default:
		return time.Time{}, fmt.Errorf("unsupported backup mode %q", cfg.Mode)
	}
}

func parseDailyAt(value string) (int, int, error) {
	var hour, minute int
	if _, err := fmt.Sscanf(strings.TrimSpace(value), "%d:%d", &hour, &minute); err != nil {
		return 0, 0, fmt.Errorf("daily_at must be HH:MM: %w", err)
	}
	if hour < 0 || hour > 23 || minute < 0 || minute > 59 {
		return 0, 0, errors.New("daily_at must be a valid time")
	}
	return hour, minute, nil
}

func timePtr(t time.Time) *time.Time { return &t }

func truncateMessage(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}
