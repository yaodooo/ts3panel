package backup

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"ts3panel/internal/model"
	"ts3panel/internal/store"
)

var (
	ErrInstanceNotFound = errors.New("instance not found")
	ErrPortConflict     = errors.New("backup ports are already used by another instance")
)

// Lifecycle is the slice of the orchestrator the backup manager needs. It is an
// interface so the archive logic can be tested without Docker.
type Lifecycle interface {
	Running(ctx context.Context, id string) (bool, error)
	Stop(ctx context.Context, id string) error
	Start(ctx context.Context, id string) error
}

// Options configures a backup Manager.
type Options struct {
	Store     *store.StateStore
	Root      string
	Lifecycle Lifecycle
	Logger    *slog.Logger

	// Now and DiskFree are test seams. Production uses time.Now and the
	// platform disk-free implementation.
	Now      func() time.Time
	DiskFree func(string) (int64, error)
}

// Manager creates and restores instance archives. Operations on one instance
// are serialized; different instances may proceed in parallel.
type Manager struct {
	store     *store.StateStore
	root      string
	lifecycle Lifecycle
	logger    *slog.Logger
	now       func() time.Time
	diskFree  func(string) (int64, error)

	locks sync.Map // map[string]*sync.Mutex
}

func New(opts Options) (*Manager, error) {
	if opts.Store == nil {
		return nil, errors.New("backup: state store is required")
	}
	if opts.Lifecycle == nil {
		return nil, errors.New("backup: lifecycle is required")
	}
	root := strings.TrimSpace(opts.Root)
	if root == "" {
		root = "/srv/ts3"
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	diskFree := opts.DiskFree
	if diskFree == nil {
		diskFree = diskFreeBytes
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &Manager{
		store:     opts.Store,
		root:      root,
		lifecycle: opts.Lifecycle,
		logger:    logger,
		now:       now,
		diskFree:  diskFree,
	}, nil
}

// BackupDir is the archive directory under the instance root.
func (m *Manager) BackupDir() string { return filepath.Join(m.root, ".backups") }

func (m *Manager) lockFor(id string) *sync.Mutex {
	value, _ := m.locks.LoadOrStore(id, &sync.Mutex{})
	return value.(*sync.Mutex)
}

// Create archives one instance. The container is stopped for the duration of
// the copy and started again even when archiving fails. The archive is written
// to a .tmp file and renamed into place, so an interrupted run cannot leave a
// file that looks like a valid backup.
func (m *Manager) Create(ctx context.Context, id string, auto bool) (rec model.BackupRecord, err error) {
	lock := m.lockFor(id)
	lock.Lock()
	defer lock.Unlock()

	st, err := m.store.Snapshot()
	if err != nil {
		return rec, err
	}
	inst, ok := findInstance(st, id)
	if !ok {
		return rec, fmt.Errorf("%w: %q", ErrInstanceNotFound, id)
	}

	dir := filepath.Join(m.root, inst.ID)
	if fi, statErr := os.Stat(dir); statErr != nil || !fi.IsDir() {
		if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
			return rec, fmt.Errorf("stat instance directory: %w", statErr)
		}
		return rec, fmt.Errorf("%w: %s", ErrInstanceDirMissing, dir)
	}

	dirSize, err := dirSizeBytes(ctx, dir)
	if err != nil {
		return rec, fmt.Errorf("measure instance directory: %w", err)
	}
	free, freeErr := m.diskFree(m.root)
	if freeErr != nil {
		// An unknown free-space value must not block the backup; it only means
		// the pre-flight check cannot run.
		m.logger.Warn("disk free check failed", "instance_id", inst.ID, "err", freeErr)
		free = -1
	}
	if free >= 0 && dirSize > 0 && free < dirSize*2 {
		return rec, &DiskSpaceError{Free: free, Needed: dirSize * 2, DirSize: dirSize}
	}

	running, err := m.lifecycle.Running(ctx, inst.ID)
	if err != nil {
		return rec, err
	}
	if running {
		if err := m.lifecycle.Stop(ctx, inst.ID); err != nil {
			return rec, fmt.Errorf("stop container before backup: %w", err)
		}
		// Restart even if archiving fails: the instance must not be left down.
		defer func() {
			if startErr := m.lifecycle.Start(ctx, inst.ID); startErr != nil {
				if err == nil {
					err = fmt.Errorf("backup finished but container restart failed: %w", startErr)
				} else {
					err = fmt.Errorf("%w (container restart also failed: %v)", err, startErr)
				}
			}
		}()
	}

	if err := os.MkdirAll(m.BackupDir(), backupDirMode); err != nil {
		return rec, fmt.Errorf("create backup directory: %w", err)
	}
	if err := os.Chmod(m.BackupDir(), backupDirMode); err != nil {
		return rec, fmt.Errorf("set backup directory mode: %w", err)
	}

	at := m.now().UTC()
	name := archiveName(inst.ID, at, func(candidate string) bool {
		for _, item := range st.Backups {
			if item.File == candidate {
				return true
			}
		}
		_, statErr := os.Stat(filepath.Join(m.BackupDir(), candidate))
		return statErr == nil
	})
	finalPath := filepath.Join(m.BackupDir(), name)
	tmpPath := filepath.Join(m.BackupDir(), "."+name+".tmp")
	_ = os.Remove(tmpPath)

	size, err := createArchive(ctx, dir, tmpPath, m.now())
	if err != nil {
		_ = os.Remove(tmpPath)
		return rec, err
	}
	if err := os.Rename(tmpPath, finalPath); err != nil {
		_ = os.Remove(tmpPath)
		return rec, fmt.Errorf("commit archive: %w", err)
	}

	rec = model.BackupRecord{
		InstanceID: inst.ID,
		File:       name,
		SizeBytes:  size,
		CreatedAt:  at,
		Auto:       auto,
		Name:       inst.Name,
		Subdomain:  inst.Subdomain,
		ProviderID: inst.ProviderID,
		Container:  inst.Container,
		Ports:      inst.Ports,
	}
	if err := m.store.Update(func(cur *store.State) error {
		cur.Backups = append(cur.Backups, rec)
		return nil
	}); err != nil {
		// The archive is useless without its record: remove it so a later run
		// does not leave an orphan file behind.
		_ = os.Remove(finalPath)
		return rec, fmt.Errorf("record backup: %w", err)
	}
	m.logger.Info("instance backed up", "instance_id", inst.ID, "file", name, "size", size, "auto", auto)
	return rec, nil
}

// Restore replaces an instance's data directory with the archive contents. The
// previous directory is kept until the container is running again; any failure
// moves it back. A backup whose instance was deleted recreates the instance
// with the original id, address and port group.
func (m *Manager) Restore(ctx context.Context, id, file string) (model.BackupRecord, error) {
	lock := m.lockFor(id)
	lock.Lock()
	defer lock.Unlock()

	if !isValidArchiveName(file) {
		return model.BackupRecord{}, fmt.Errorf("%w: %q", ErrBackupNotFound, file)
	}
	st, err := m.store.Snapshot()
	if err != nil {
		return model.BackupRecord{}, err
	}
	rec, ok := findBackup(st, id, file)
	if !ok {
		return model.BackupRecord{}, fmt.Errorf("%w: %s", ErrBackupNotFound, file)
	}
	inst, exists := findInstance(st, id)
	if exists {
		for _, other := range st.Instances {
			if other.ID != id && other.Ports == inst.Ports {
				return model.BackupRecord{}, fmt.Errorf("%w: %s", ErrPortConflict, other.ID)
			}
		}
	} else {
		// Restoring a deleted instance: the snapshot in the record decides the
		// id, address and ports, so the TS3 identity and the client bookmark
		// stay valid.
		for _, other := range st.Instances {
			if other.Ports == rec.Ports {
				return model.BackupRecord{}, fmt.Errorf("%w: %s", ErrPortConflict, other.ID)
			}
		}
		inst = model.Instance{
			ID:         rec.InstanceID,
			Name:       rec.Name,
			Subdomain:  rec.Subdomain,
			ProviderID: rec.ProviderID,
			Ports:      rec.Ports,
			Container:  rec.Container,
			Status:     model.InstanceStatusStopped,
			DNSStatus:  model.DNSStatusNone,
			CreatedAt:  m.now().UTC(),
		}
		if inst.Name == "" {
			inst.Name = inst.ID
		}
		if inst.Container == "" {
			inst.Container = "ts3-" + inst.ID
		}
	}

	archivePath := filepath.Join(m.BackupDir(), file)
	if _, err := inspectArchive(archivePath); err != nil {
		return model.BackupRecord{}, err
	}

	dir := filepath.Join(m.root, inst.ID)

	// The container is stopped before extraction so the live SQLite database is
	// never written while it is being replaced. It is started again on every
	// failure path below.
	wasRunning := false
	if exists {
		running, runErr := m.lifecycle.Running(ctx, inst.ID)
		if runErr != nil {
			return model.BackupRecord{}, runErr
		}
		if running {
			if err := m.lifecycle.Stop(ctx, inst.ID); err != nil {
				return model.BackupRecord{}, fmt.Errorf("stop container before restore: %w", err)
			}
			wasRunning = true
		}
	}

	staging := filepath.Join(m.root, fmt.Sprintf(".restore-%s-%d", inst.ID, m.now().UnixNano()))
	rollback := filepath.Join(m.root, fmt.Sprintf(".restore-old-%s-%d", inst.ID, m.now().UnixNano()))
	if err := os.MkdirAll(staging, 0o700); err != nil {
		m.restartIfRunning(ctx, inst.ID, wasRunning)
		return model.BackupRecord{}, fmt.Errorf("create restore staging directory: %w", err)
	}
	stagingReady := false
	defer func() {
		if !stagingReady {
			_ = os.RemoveAll(staging)
		}
	}()
	if err := extractArchive(ctx, archivePath, staging); err != nil {
		m.restartIfRunning(ctx, inst.ID, wasRunning)
		return model.BackupRecord{}, err
	}

	hadDir := false
	if _, statErr := os.Stat(dir); statErr == nil {
		if err := os.Rename(dir, rollback); err != nil {
			m.restartIfRunning(ctx, inst.ID, wasRunning)
			return model.BackupRecord{}, fmt.Errorf("move old data aside: %w", err)
		}
		hadDir = true
	} else if !errors.Is(statErr, os.ErrNotExist) {
		m.restartIfRunning(ctx, inst.ID, wasRunning)
		return model.BackupRecord{}, fmt.Errorf("stat instance directory: %w", statErr)
	}

	if err := os.Rename(staging, dir); err != nil {
		if hadDir {
			_ = os.Rename(rollback, dir)
		}
		m.restartIfRunning(ctx, inst.ID, wasRunning)
		return model.BackupRecord{}, fmt.Errorf("install restored data: %w", err)
	}
	stagingReady = true

	if !exists {
		if err := m.store.Update(func(cur *store.State) error {
			for _, other := range cur.Instances {
				if other.ID == inst.ID {
					return fmt.Errorf("%w: %q", ErrInstanceNotFound, inst.ID)
				}
				if other.Ports == inst.Ports {
					return fmt.Errorf("%w: %s", ErrPortConflict, other.ID)
				}
			}
			cur.Instances = append(cur.Instances, inst)
			return nil
		}); err != nil {
			_ = os.RemoveAll(dir)
			if hadDir {
				_ = os.Rename(rollback, dir)
			}
			return model.BackupRecord{}, err
		}
	}

	if err := m.lifecycle.Start(ctx, inst.ID); err != nil {
		// Put the previous data back and restart it, so a failed restore does
		// not destroy a working instance.
		_ = os.RemoveAll(dir)
		if hadDir {
			_ = os.Rename(rollback, dir)
		}
		if !exists {
			_ = m.store.Update(func(cur *store.State) error {
				out := cur.Instances[:0]
				for _, item := range cur.Instances {
					if item.ID != inst.ID {
						out = append(out, item)
					}
				}
				cur.Instances = out
				return nil
			})
		}
		m.restartIfRunning(ctx, inst.ID, wasRunning)
		return model.BackupRecord{}, fmt.Errorf("start container after restore: %w", err)
	}

	if hadDir {
		if err := os.RemoveAll(rollback); err != nil {
			m.logger.Warn("remove restore rollback directory failed", "instance_id", inst.ID, "err", err)
		}
	}
	m.logger.Info("instance restored", "instance_id", inst.ID, "file", file, "recreated", !exists)
	return rec, nil
}

func (m *Manager) restartIfRunning(ctx context.Context, id string, wasRunning bool) {
	if !wasRunning {
		return
	}
	if err := m.lifecycle.Start(ctx, id); err != nil {
		m.logger.Error("restart after failed restore failed", "instance_id", id, "err", err)
	}
}

// Prune removes automatic archives beyond keep for one instance. Manual
// backups are never deleted by the schedule: the operator asked for them.
func (m *Manager) Prune(ctx context.Context, id string, keep int) (int, error) {
	if keep < 1 {
		keep = 1
	}
	lock := m.lockFor(id)
	lock.Lock()
	defer lock.Unlock()

	st, err := m.store.Snapshot()
	if err != nil {
		return 0, err
	}
	var autos []model.BackupRecord
	for _, rec := range st.Backups {
		if rec.InstanceID == id && rec.Auto {
			autos = append(autos, rec)
		}
	}
	if len(autos) <= keep {
		return 0, nil
	}
	sort.Slice(autos, func(i, j int) bool { return autos[i].CreatedAt.After(autos[j].CreatedAt) })
	extras := autos[keep:]
	remove := make(map[string]bool, len(extras))
	for _, rec := range extras {
		remove[rec.File] = true
		if !isValidArchiveName(rec.File) {
			continue
		}
		if err := os.Remove(filepath.Join(m.BackupDir(), rec.File)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return 0, fmt.Errorf("remove old backup %s: %w", rec.File, err)
		}
	}
	if err := m.store.Update(func(cur *store.State) error {
		out := cur.Backups[:0]
		for _, rec := range cur.Backups {
			if remove[rec.File] {
				continue
			}
			out = append(out, rec)
		}
		cur.Backups = out
		return nil
	}); err != nil {
		return 0, err
	}
	m.logger.Info("old backups pruned", "instance_id", id, "removed", len(extras), "keep", keep)
	return len(extras), nil
}

// List returns the archives of one instance, newest first.
func (m *Manager) List(id string) ([]model.BackupRecord, error) {
	st, err := m.store.Snapshot()
	if err != nil {
		return nil, err
	}
	out := make([]model.BackupRecord, 0)
	for _, rec := range st.Backups {
		if rec.InstanceID == id {
			out = append(out, rec)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out, nil
}

// All returns every archive, newest first.
func (m *Manager) All() ([]model.BackupRecord, error) {
	st, err := m.store.Snapshot()
	if err != nil {
		return nil, err
	}
	out := append([]model.BackupRecord(nil), st.Backups...)
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out, nil
}

// Open returns an opened archive for streaming. Only records known to the state
// are accepted, which also prevents path traversal through a crafted file name.
func (m *Manager) Open(id, file string) (*os.File, model.BackupRecord, error) {
	if !isValidArchiveName(file) {
		return nil, model.BackupRecord{}, fmt.Errorf("%w: %q", ErrBackupNotFound, file)
	}
	st, err := m.store.Snapshot()
	if err != nil {
		return nil, model.BackupRecord{}, err
	}
	rec, ok := findBackup(st, id, file)
	if !ok {
		return nil, model.BackupRecord{}, fmt.Errorf("%w: %s", ErrBackupNotFound, file)
	}
	f, err := os.Open(filepath.Join(m.BackupDir(), rec.File))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, rec, ErrArchiveMissing
		}
		return nil, rec, err
	}
	return f, rec, nil
}

// ImportTempFile moves an uploaded archive into the backup directory and records
// it for the instance. The caller must already have validated the upload; this
// method validates it again before committing.
func (m *Manager) ImportTempFile(ctx context.Context, id, tmpPath string) (model.BackupRecord, error) {
	if _, err := inspectArchive(tmpPath); err != nil {
		return model.BackupRecord{}, err
	}
	st, err := m.store.Snapshot()
	if err != nil {
		return model.BackupRecord{}, err
	}
	inst, ok := findInstance(st, id)
	if !ok {
		return model.BackupRecord{}, fmt.Errorf("%w: %q", ErrInstanceNotFound, id)
	}
	if err := os.MkdirAll(m.BackupDir(), backupDirMode); err != nil {
		return model.BackupRecord{}, err
	}
	at := m.now().UTC()
	name := archiveName(inst.ID, at, func(candidate string) bool {
		for _, item := range st.Backups {
			if item.File == candidate {
				return true
			}
		}
		_, statErr := os.Stat(filepath.Join(m.BackupDir(), candidate))
		return statErr == nil
	})
	finalPath := filepath.Join(m.BackupDir(), name)
	if err := os.Rename(tmpPath, finalPath); err != nil {
		// Cross-device uploads (temp on another filesystem) fall back to copy.
		if copyErr := copyFile(tmpPath, finalPath); copyErr != nil {
			return model.BackupRecord{}, fmt.Errorf("store uploaded archive: %w", copyErr)
		}
		_ = os.Remove(tmpPath)
	}
	if err := os.Chmod(finalPath, backupFileMode); err != nil {
		return model.BackupRecord{}, err
	}
	fi, err := os.Stat(finalPath)
	if err != nil {
		return model.BackupRecord{}, err
	}
	rec := model.BackupRecord{
		InstanceID: inst.ID,
		File:       name,
		SizeBytes:  fi.Size(),
		CreatedAt:  at,
		Auto:       false,
		Name:       inst.Name,
		Subdomain:  inst.Subdomain,
		ProviderID: inst.ProviderID,
		Container:  inst.Container,
		Ports:      inst.Ports,
	}
	if err := m.store.Update(func(cur *store.State) error {
		cur.Backups = append(cur.Backups, rec)
		return nil
	}); err != nil {
		_ = os.Remove(finalPath)
		return model.BackupRecord{}, err
	}
	return rec, nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, backupFileMode)
	if err != nil {
		return err
	}
	if _, err := out.ReadFrom(in); err != nil {
		_ = out.Close()
		_ = os.Remove(dst)
		return err
	}
	if err := out.Sync(); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

func findInstance(st store.State, id string) (model.Instance, bool) {
	for _, inst := range st.Instances {
		if inst.ID == id {
			return inst, true
		}
	}
	return model.Instance{}, false
}

func findBackup(st store.State, id, file string) (model.BackupRecord, bool) {
	for _, rec := range st.Backups {
		if rec.InstanceID == id && rec.File == file {
			return rec, true
		}
	}
	return model.BackupRecord{}, false
}
