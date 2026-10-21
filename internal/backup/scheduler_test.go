package backup

import (
	"path/filepath"
	"testing"
	"time"

	"ts3panel/internal/model"
	"ts3panel/internal/store"
)

func newSchedulerEnv(t *testing.T) (*Scheduler, *store.StateStore, *store.ConfigStore, *fakeLifecycle) {
	t.Helper()
	dir := t.TempDir()
	cfgStore, _, err := store.LoadConfig(filepath.Join(dir, "config.toml"))
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if err := cfgStore.Update(func(cfg *store.Config) error {
		cfg.Backup.Enabled = true
		cfg.Backup.Mode = "daily"
		cfg.Backup.DailyAt = "02:00"
		cfg.Backup.Retention = 7
		return nil
	}); err != nil {
		t.Fatalf("save config: %v", err)
	}
	stateStore, err := store.OpenState(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatalf("open state: %v", err)
	}
	if err := stateStore.Update(func(st *store.State) error {
		st.Instances = []model.Instance{testInstance()}
		return nil
	}); err != nil {
		t.Fatalf("seed state: %v", err)
	}
	root := filepath.Join(dir, "instances")
	writeFile(t, filepath.Join(root, "ts1", "ts3server.sqlitedb"), []byte("db"), 0o644, time.Now())

	clock := time.Date(2026, 10, 22, 3, 0, 0, 0, time.UTC)
	lifecycle := &fakeLifecycle{}
	mgr, err := New(Options{
		Store:     stateStore,
		Root:      root,
		Lifecycle: lifecycle,
		Now:       func() time.Time { return clock },
		DiskFree:  func(string) (int64, error) { return 10 << 30, nil },
	})
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	sched, err := NewScheduler(SchedulerOptions{
		Config:  cfgStore,
		State:   stateStore,
		Manager: mgr,
		Now:     func() time.Time { return clock },
	})
	if err != nil {
		t.Fatalf("new scheduler: %v", err)
	}
	return sched, stateStore, cfgStore, lifecycle
}

func TestComputeNextDaily(t *testing.T) {
	cfg := store.BackupConfig{Mode: "daily", DailyAt: "04:30"}
	now := time.Date(2026, 10, 22, 5, 0, 0, 0, time.UTC)
	next, err := computeNext(now, cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	if want := time.Date(2026, 10, 23, 4, 30, 0, 0, time.UTC); !next.Equal(want) {
		t.Fatalf("next = %v, want %v", next, want)
	}

	last := time.Date(2026, 10, 21, 4, 30, 0, 0, time.UTC)
	next, err = computeNext(now, cfg, &last)
	if err != nil {
		t.Fatal(err)
	}
	if want := time.Date(2026, 10, 22, 4, 30, 0, 0, time.UTC); !next.Equal(want) {
		t.Fatalf("catch-up next = %v, want %v", next, want)
	}
}

func TestComputeNextInterval(t *testing.T) {
	cfg := store.BackupConfig{Mode: "interval", IntervalHours: 6}
	now := time.Date(2026, 10, 22, 3, 0, 0, 0, time.UTC)
	next, err := computeNext(now, cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	if want := now.Add(6 * time.Hour); !next.Equal(want) {
		t.Fatalf("next = %v, want %v", next, want)
	}
	last := now.Add(-7 * time.Hour)
	next, err = computeNext(now, cfg, &last)
	if err != nil {
		t.Fatal(err)
	}
	if want := last.Add(6 * time.Hour); !next.Equal(want) {
		t.Fatalf("catch-up next = %v, want %v", next, want)
	}
}

func TestSchedulerRunsDueBackupAndDoesNotRepeat(t *testing.T) {
	sched, stateStore, _, _ := newSchedulerEnv(t)

	past := time.Date(2026, 10, 22, 2, 0, 0, 0, time.UTC)
	if err := stateStore.Update(func(st *store.State) error {
		st.Backup.NextRunAt = &past
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	sched.tickOnce()
	st, _ := stateStore.Snapshot()
	if len(st.Backups) != 1 || !st.Backups[0].Auto {
		t.Fatalf("backups = %+v, want one automatic backup", st.Backups)
	}
	if st.Backup.LastSuccessAt == nil || !st.Backup.LastSuccessAt.Equal(time.Date(2026, 10, 22, 3, 0, 0, 0, time.UTC)) {
		t.Fatalf("last success = %v", st.Backup.LastSuccessAt)
	}
	if st.Backup.NextRunAt == nil || !st.Backup.NextRunAt.After(time.Date(2026, 10, 22, 3, 0, 0, 0, time.UTC)) {
		t.Fatalf("next run = %v, want a future time", st.Backup.NextRunAt)
	}

	// A restart at the same instant re-reads the persisted next run and must
	// not produce a second archive.
	sched.tickOnce()
	st, _ = stateStore.Snapshot()
	if len(st.Backups) != 1 {
		t.Fatalf("backups = %+v, want no duplicate run", st.Backups)
	}
}

func TestSchedulerDisabledDoesNothing(t *testing.T) {
	sched, stateStore, cfgStore, _ := newSchedulerEnv(t)
	if err := cfgStore.Update(func(cfg *store.Config) error {
		cfg.Backup.Enabled = false
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	past := time.Date(2026, 10, 22, 2, 0, 0, 0, time.UTC)
	if err := stateStore.Update(func(st *store.State) error {
		st.Backup.NextRunAt = &past
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	sched.tickOnce()
	st, _ := stateStore.Snapshot()
	if len(st.Backups) != 0 {
		t.Fatalf("backups = %+v, want none", st.Backups)
	}
	if st.Backup.NextRunAt != nil {
		t.Fatalf("next run = %v, want cleared", st.Backup.NextRunAt)
	}
}

func TestSchedulerPrunesToRetention(t *testing.T) {
	sched, stateStore, cfgStore, _ := newSchedulerEnv(t)
	if err := cfgStore.Update(func(cfg *store.Config) error {
		cfg.Backup.Retention = 2
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	base := time.Date(2026, 10, 22, 2, 0, 0, 0, time.UTC)
	clock := base
	sched.now = func() time.Time { return clock }

	for i := 0; i < 3; i++ {
		clock = base.Add(time.Duration(i) * 24 * time.Hour)
		due := clock.Add(-time.Minute)
		if err := stateStore.Update(func(st *store.State) error {
			st.Backup.NextRunAt = &due
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		sched.tickOnce()
	}

	st, _ := stateStore.Snapshot()
	if len(st.Backups) != 2 {
		t.Fatalf("backups = %d, want 2 after retention pruning", len(st.Backups))
	}
}

func TestSchedulerCatchUpAfterDowntime(t *testing.T) {
	sched, stateStore, _, _ := newSchedulerEnv(t)
	last := time.Date(2026, 10, 20, 2, 0, 0, 0, time.UTC)
	next := time.Date(2026, 10, 21, 2, 0, 0, 0, time.UTC)
	if err := stateStore.Update(func(st *store.State) error {
		st.Backup.LastSuccessAt = &last
		st.Backup.NextRunAt = &next
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	sched.tickOnce()
	st, _ := stateStore.Snapshot()
	if len(st.Backups) != 1 {
		t.Fatalf("backups = %d, want one catch-up run", len(st.Backups))
	}
}

func TestSchedulerReconcilePersistsNextRun(t *testing.T) {
	sched, stateStore, _, _ := newSchedulerEnv(t)
	if err := sched.Reconcile(); err != nil {
		t.Fatal(err)
	}
	st, _ := stateStore.Snapshot()
	want := time.Date(2026, 10, 23, 2, 0, 0, 0, time.UTC)
	if st.Backup.NextRunAt == nil || !st.Backup.NextRunAt.Equal(want) {
		t.Fatalf("next run = %v, want %v", st.Backup.NextRunAt, want)
	}
}
