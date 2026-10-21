package store

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"ts3panel/internal/model"
)

// CurrentStateVersion is the schema version written by this build.
const CurrentStateVersion = 2

// SetupToken is a hash-only record for the CLI reset link. The plaintext token
// is printed once by `ts3panel reset-password` and never stored.
type SetupToken struct {
	Hash      string    `json:"hash"`
	ExpiresAt time.Time `json:"expires_at"`
}

// State is the contents of state.json. The store caches it in memory and uses
// the file's mtime+size to notice writes from another process (such as the
// root-only reset-password CLI) without re-parsing on every request.
type State struct {
	Version         int                  `json:"version"`
	SetupCompleted  bool                 `json:"setup_completed"`
	SetupDNSSkipped bool                 `json:"setup_dns_skipped,omitempty"`
	User            *model.User          `json:"user,omitempty"`
	RecoveryCodes   []model.RecoveryCode `json:"recovery_codes"`
	Instances       []model.Instance     `json:"instances"`
	NextIndex       int                  `json:"next_index"`
	SetupToken      *SetupToken          `json:"setup_token,omitempty"`
	// Backups survives instance deletion on purpose: a purged instance must be
	// recoverable from its archive.
	Backups []model.BackupRecord `json:"backups,omitempty"`
	Backup  model.BackupRunState `json:"backup,omitempty"`
}

// StateStore serializes state.json reads and writes with a process-wide mutex.
// Writes are temp file -> fsync -> rename, so readers never observe a partial
// document. Reads are served from cached until the file's mtime or size changes.
type StateStore struct {
	mu            sync.Mutex
	path          string
	cached        State
	cachedModTime time.Time
	cachedSize    int64
	loaded        bool
}

// OpenState opens state.json, creating a fresh version-2 file when missing.
func OpenState(path string) (*StateStore, error) {
	return openState(path, true)
}

// OpenExistingState opens state.json and fails when missing. The CLI uses this
// so reset-password cannot silently create a root-owned state file that the
// service user cannot read.
func OpenExistingState(path string) (*StateStore, error) {
	return openState(path, false)
}

func openState(path string, create bool) (*StateStore, error) {
	if path == "" {
		path = DefaultStatePath
	}
	path = filepath.Clean(path)
	s := &StateStore{path: path}

	s.mu.Lock()
	defer s.mu.Unlock()

	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		if !create {
			return nil, fmt.Errorf("state file %s does not exist", path)
		}
		if err := s.saveLocked(initialState()); err != nil {
			return nil, err
		}
		return s, nil
	} else if err != nil {
		return nil, fmt.Errorf("stat state file: %w", err)
	}

	if _, err := s.loadIfChangedLocked(); err != nil {
		return nil, err
	}
	return s, nil
}

func initialState() State {
	return State{
		Version:        CurrentStateVersion,
		SetupCompleted: false,
		RecoveryCodes:  []model.RecoveryCode{},
		Instances:      []model.Instance{},
		NextIndex:      0,
	}
}

// Snapshot returns a copy of the cached state, reloading it first when the
// underlying file's mtime or size changed.
func (s *StateStore) Snapshot() (State, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, err := s.loadIfChangedLocked(); err != nil {
		return State{}, err
	}
	return cloneState(s.cached), nil
}

// Update refreshes the cache, applies fn to a copy, validates the result and
// atomically persists it. If fn or validation/save fails, both the file and the
// cache are left unchanged.
func (s *StateStore) Update(fn func(*State) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, err := s.loadIfChangedLocked(); err != nil {
		return err
	}
	next := cloneState(s.cached)
	if err := fn(&next); err != nil {
		return err
	}
	normalizeState(&next)
	if err := validateState(&next); err != nil {
		return err
	}
	return s.saveLocked(next)
}

func (s *StateStore) Path() string {
	return s.path
}

// loadIfChangedLocked returns reloaded=true when the file was re-read. A hit on
// mtime+size is deliberately cheap: no JSON decoding or syscall beyond stat.
func (s *StateStore) loadIfChangedLocked() (bool, error) {
	fi, err := os.Stat(s.path)
	if err != nil {
		return false, fmt.Errorf("stat state file: %w", err)
	}
	if s.loaded && fi.ModTime().Equal(s.cachedModTime) && fi.Size() == s.cachedSize {
		return false, nil
	}

	data, err := os.ReadFile(s.path)
	if err != nil {
		return false, fmt.Errorf("read state file: %w", err)
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return false, errors.New("state file is empty")
	}
	var st State
	if err := json.Unmarshal(data, &st); err != nil {
		return false, fmt.Errorf("parse state file: %w", err)
	}
	normalizeState(&st)
	changed, err := migrateState(&st)
	if err != nil {
		return false, err
	}
	normalizeState(&st)
	if err := validateState(&st); err != nil {
		return false, err
	}
	if changed {
		if err := s.saveLocked(st); err != nil {
			return false, err
		}
		return true, nil
	}
	s.setCachedLocked(st, fi)
	return true, nil
}

func (s *StateStore) setCachedLocked(st State, fi os.FileInfo) {
	s.cached = cloneState(st)
	s.cachedModTime = fi.ModTime()
	s.cachedSize = fi.Size()
	s.loaded = true
}

func cloneState(st State) State {
	out := st
	if st.User != nil {
		user := *st.User
		out.User = &user
	}
	if st.RecoveryCodes == nil {
		out.RecoveryCodes = []model.RecoveryCode{}
	} else {
		out.RecoveryCodes = make([]model.RecoveryCode, len(st.RecoveryCodes))
		copy(out.RecoveryCodes, st.RecoveryCodes)
		for i := range out.RecoveryCodes {
			if st.RecoveryCodes[i].UsedAt != nil {
				usedAt := *st.RecoveryCodes[i].UsedAt
				out.RecoveryCodes[i].UsedAt = &usedAt
			}
		}
	}
	if st.Instances == nil {
		out.Instances = []model.Instance{}
	} else {
		out.Instances = make([]model.Instance, len(st.Instances))
		copy(out.Instances, st.Instances)
		for i := range out.Instances {
			if st.Instances[i].DNS.VerifiedAt != nil {
				verifiedAt := *st.Instances[i].DNS.VerifiedAt
				out.Instances[i].DNS.VerifiedAt = &verifiedAt
			}
		}
	}
	if st.SetupToken != nil {
		token := *st.SetupToken
		out.SetupToken = &token
	}
	if st.Backups == nil {
		out.Backups = []model.BackupRecord{}
	} else {
		out.Backups = make([]model.BackupRecord, len(st.Backups))
		copy(out.Backups, st.Backups)
	}
	out.Backup = cloneBackupRunState(st.Backup)
	return out
}

func cloneBackupRunState(in model.BackupRunState) model.BackupRunState {
	out := in
	if in.LastAttemptAt != nil {
		v := *in.LastAttemptAt
		out.LastAttemptAt = &v
	}
	if in.LastSuccessAt != nil {
		v := *in.LastSuccessAt
		out.LastSuccessAt = &v
	}
	if in.NextRunAt != nil {
		v := *in.NextRunAt
		out.NextRunAt = &v
	}
	return out
}

func normalizeState(st *State) {
	if st.RecoveryCodes == nil {
		st.RecoveryCodes = []model.RecoveryCode{}
	}
	if st.Instances == nil {
		st.Instances = []model.Instance{}
	}
	if st.Backups == nil {
		st.Backups = []model.BackupRecord{}
	}
	if st.NextIndex < 0 {
		st.NextIndex = 0
	}
}

// migrateState is deliberately a skeleton: v1 has no extra fields to map yet,
// but the version gate is in place so later migrations cannot be skipped.
func migrateState(st *State) (bool, error) {
	changed := false
	if st.Version == 0 {
		st.Version = 1
		changed = true
	}
	if st.Version > CurrentStateVersion {
		return changed, fmt.Errorf("state version %d is newer than supported version %d", st.Version, CurrentStateVersion)
	}
	for st.Version < CurrentStateVersion {
		switch st.Version {
		case 1:
			// v1 -> v2: schema skeleton. Later versions will map legacy fields here.
			st.Version = 2
			changed = true
		default:
			return changed, fmt.Errorf("no migration path from state version %d", st.Version)
		}
	}
	return changed, nil
}

func validateState(st *State) error {
	if st.Version != CurrentStateVersion {
		return fmt.Errorf("state version is %d, expected %d", st.Version, CurrentStateVersion)
	}
	if st.SetupCompleted && st.User == nil {
		return errors.New("state is inconsistent: setup_completed is true without a user")
	}
	if st.SetupCompleted && st.User != nil && st.User.Username == "" {
		return errors.New("state is inconsistent: user has empty username")
	}
	if st.NextIndex < 0 {
		return errors.New("state is inconsistent: next_index is negative")
	}
	seenFiles := make(map[string]bool, len(st.Backups))
	for _, rec := range st.Backups {
		if rec.InstanceID == "" || rec.File == "" {
			return errors.New("state is inconsistent: backup record without instance id or file name")
		}
		if seenFiles[rec.File] {
			return fmt.Errorf("state is inconsistent: duplicate backup file %q", rec.File)
		}
		seenFiles[rec.File] = true
	}
	return nil
}

func (s *StateStore) saveLocked(st State) error {
	normalizeState(&st)
	if err := validateState(&st); err != nil {
		return err
	}
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return fmt.Errorf("encode state: %w", err)
	}
	data = append(data, '\n')
	if err := writeFileAtomic(s.path, data, 0o600); err != nil {
		return fmt.Errorf("write state: %w", err)
	}
	fi, err := os.Stat(s.path)
	if err != nil {
		// The write succeeded; force a reload on the next read.
		s.loaded = false
		return nil
	}
	s.setCachedLocked(st, fi)
	return nil
}

// writeFileAtomic writes data to a temp file in the destination directory,
// fsyncs it, and renames it over path. Preserving the previous owner is
// required because reset-password runs as root while the service runs as an
// unprivileged user; a root-owned state file would break the service.
func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create directory %s: %w", dir, err)
	}

	var oldInfo os.FileInfo
	if fi, err := os.Stat(path); err == nil {
		oldInfo = fi
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("stat existing file: %w", err)
	}

	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpName := tmp.Name()
	committed := false
	defer func() {
		if !committed {
			_ = os.Remove(tmpName)
		}
	}()

	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("chmod temp file: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temp file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("fsync temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp file: %w", err)
	}

	if oldInfo != nil {
		if err := preserveOwner(tmpName, oldInfo); err != nil {
			return fmt.Errorf("preserve file owner: %w", err)
		}
	}

	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("rename temp file: %w", err)
	}
	committed = true
	syncDir(dir)
	return nil
}

// syncDir is best-effort: some filesystems (and Windows) do not support fsync
// on directories. The file itself has already been fsynced.
func syncDir(dir string) {
	d, err := os.Open(dir)
	if err != nil {
		return
	}
	defer d.Close()
	_ = d.Sync()
}
