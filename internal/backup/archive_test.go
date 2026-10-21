package backup

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"ts3panel/internal/model"
	"ts3panel/internal/store"
)

// fakeLifecycle records stop/start calls without touching Docker.
type fakeLifecycle struct {
	running  bool
	startErr error
	stops    int
	starts   int
}

func (f *fakeLifecycle) Running(context.Context, string) (bool, error) { return f.running, nil }
func (f *fakeLifecycle) Stop(context.Context, string) error {
	f.stops++
	f.running = false
	return nil
}
func (f *fakeLifecycle) Start(context.Context, string) error {
	f.starts++
	if f.startErr != nil {
		return f.startErr
	}
	f.running = true
	return nil
}

func writeFile(t *testing.T, path string, data []byte, mode os.FileMode, mtime time.Time) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, mode); err != nil {
		t.Fatal(err)
	}
	if !mtime.IsZero() {
		if err := os.Chtimes(path, mtime, mtime); err != nil {
			t.Fatal(err)
		}
	}
}

func testInstance() model.Instance {
	return model.Instance{
		ID:        "ts1",
		Name:      "一队",
		Subdomain: "ts1.example.com",
		Ports:     model.Ports{Voice: 9987, FileTransfer: 30033, Query: 10011, QuerySSH: 10022},
		Container: "ts3-ts1",
		Status:    model.InstanceStatusReady,
		DNSStatus: model.DNSStatusNone,
		CreatedAt: time.Now().UTC(),
	}
}

func newTestEnv(t *testing.T) (*Manager, *store.StateStore, string, *fakeLifecycle) {
	t.Helper()
	dir := t.TempDir()
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
	instDir := filepath.Join(root, "ts1")
	writeFile(t, filepath.Join(instDir, "ts3server.sqlitedb"), []byte("sqlite-identity-v1"), 0o644, time.Now())
	writeFile(t, filepath.Join(instDir, "files", "icon.bin"), []byte("icon-bytes"), 0o644, time.Now())
	writeFile(t, filepath.Join(instDir, "logs", "ts3server_2026-10-20.log"), []byte("old log"), 0o644, time.Now().Add(-30*24*time.Hour))
	writeFile(t, filepath.Join(instDir, "logs", "ts3server_2026-10-22.log"), []byte("new log"), 0o644, time.Now())

	lifecycle := &fakeLifecycle{running: true}
	mgr, err := New(Options{
		Store:     stateStore,
		Root:      root,
		Lifecycle: lifecycle,
		Now:       func() time.Time { return time.Date(2026, 10, 22, 4, 30, 0, 0, time.UTC) },
		DiskFree:  func(string) (int64, error) { return 10 << 30, nil },
	})
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	return mgr, stateStore, root, lifecycle
}

func archiveEntries(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read archive: %v", err)
	}
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("gzip: %v", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	var names []string
	for {
		hdr, err := tr.Next()
		if err != nil {
			break
		}
		names = append(names, hdr.Name)
	}
	return names
}

func TestCreateBackupArchivesIdentityAndSkipsStaleLogs(t *testing.T) {
	mgr, stateStore, root, lifecycle := newTestEnv(t)

	rec, err := mgr.Create(context.Background(), "ts1", false)
	if err != nil {
		t.Fatalf("create backup: %v", err)
	}
	if !isValidArchiveName(rec.File) {
		t.Fatalf("file name %q does not contain instance id and timestamp", rec.File)
	}
	if lifecycle.stops != 1 || lifecycle.starts != 1 {
		t.Fatalf("stops=%d starts=%d, want 1/1", lifecycle.stops, lifecycle.starts)
	}
	if !lifecycle.running {
		t.Fatal("container was not restarted after backup")
	}

	archivePath := filepath.Join(root, ".backups", rec.File)
	fi, err := os.Stat(archivePath)
	if err != nil {
		t.Fatalf("stat archive: %v", err)
	}
	if runtime.GOOS != "windows" {
		if fi.Mode().Perm() != 0o600 {
			t.Fatalf("archive mode = %o, want 600", fi.Mode().Perm())
		}
		dirInfo, err := os.Stat(filepath.Join(root, ".backups"))
		if err != nil {
			t.Fatal(err)
		}
		if dirInfo.Mode().Perm() != 0o700 {
			t.Fatalf(".backups mode = %o, want 700", dirInfo.Mode().Perm())
		}
	}

	names := archiveEntries(t, archivePath)
	joined := strings.Join(names, "\n")
	if !strings.Contains(joined, "ts3server.sqlitedb") || !strings.Contains(joined, "files/icon.bin") {
		t.Fatalf("archive misses identity/files: %v", names)
	}
	if strings.Contains(joined, "2026-10-20") {
		t.Fatalf("archive contains a log older than 7 days: %v", names)
	}
	if !strings.Contains(joined, "logs/ts3server_2026-10-22.log") {
		t.Fatalf("archive misses the recent log: %v", names)
	}

	st, err := stateStore.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Backups) != 1 || st.Backups[0].File != rec.File || !st.Backups[0].CreatedAt.Equal(rec.CreatedAt) {
		t.Fatalf("state backups = %+v", st.Backups)
	}
	if st.Backups[0].Ports != testInstance().Ports || st.Backups[0].Name != "一队" {
		t.Fatalf("snapshot metadata = %+v", st.Backups[0])
	}
}

func TestCreateBackupCleansUpWhenInterrupted(t *testing.T) {
	mgr, stateStore, root, lifecycle := newTestEnv(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := mgr.Create(ctx, "ts1", false); err == nil {
		t.Fatal("canceled backup succeeded")
	}
	entries, err := os.ReadDir(filepath.Join(root, ".backups"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.Contains(entry.Name(), ".tmp") {
			t.Fatalf("temporary archive %q was left behind", entry.Name())
		}
	}
	st, _ := stateStore.Snapshot()
	if len(st.Backups) != 0 {
		t.Fatalf("state backups = %+v, want none", st.Backups)
	}
	if !lifecycle.running {
		t.Fatal("container was not restarted after an interrupted backup")
	}
}

func TestCreateBackupRefusesWithoutDiskSpace(t *testing.T) {
	mgr, stateStore, root, lifecycle := newTestEnv(t)
	mgr.diskFree = func(string) (int64, error) { return 10, nil }

	_, err := mgr.Create(context.Background(), "ts1", false)
	var diskErr *DiskSpaceError
	if !errors.As(err, &diskErr) {
		t.Fatalf("err = %v, want DiskSpaceError", err)
	}
	if lifecycle.stops != 0 {
		t.Fatal("container was stopped before the disk check")
	}
	entries, _ := os.ReadDir(filepath.Join(root, ".backups"))
	if len(entries) != 0 {
		t.Fatalf("backup directory = %v, want empty", entries)
	}
	st, _ := stateStore.Snapshot()
	if len(st.Backups) != 0 {
		t.Fatalf("state backups = %+v, want none", st.Backups)
	}
}

func TestCreateBackupDoesNotStartAStoppedInstance(t *testing.T) {
	mgr, _, _, lifecycle := newTestEnv(t)
	lifecycle.running = false

	if _, err := mgr.Create(context.Background(), "ts1", true); err != nil {
		t.Fatalf("create: %v", err)
	}
	if lifecycle.stops != 0 || lifecycle.starts != 0 {
		t.Fatalf("stops=%d starts=%d, want 0/0", lifecycle.stops, lifecycle.starts)
	}
}

func TestRestoreReplacesDataAndStartsContainer(t *testing.T) {
	mgr, stateStore, root, lifecycle := newTestEnv(t)
	rec, err := mgr.Create(context.Background(), "ts1", false)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := mgr.Restore(context.Background(), "ts1", rec.File); err != nil {
		t.Fatalf("restore: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(root, "ts1", "ts3server.sqlitedb"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "sqlite-identity-v1" {
		t.Fatalf("identity bytes = %q", data)
	}
	if !lifecycle.running {
		t.Fatal("container not running after restore")
	}
	st, _ := stateStore.Snapshot()
	if len(st.Instances) != 1 || st.Instances[0].ID != "ts1" {
		t.Fatalf("instances = %+v", st.Instances)
	}
}

func TestRestoreRollsBackWhenStartFails(t *testing.T) {
	mgr, _, root, lifecycle := newTestEnv(t)
	rec, err := mgr.Create(context.Background(), "ts1", false)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// Change the live data after the backup; a failed restore must leave it in
	// place rather than the backup contents.
	writeFile(t, filepath.Join(root, "ts1", "ts3server.sqlitedb"), []byte("live-changed"), 0o644, time.Now())
	lifecycle.startErr = errors.New("boom")

	if _, err := mgr.Restore(context.Background(), "ts1", rec.File); err == nil {
		t.Fatal("restore succeeded despite a failing start")
	}
	data, err := os.ReadFile(filepath.Join(root, "ts1", "ts3server.sqlitedb"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "live-changed" {
		t.Fatalf("data = %q, want the pre-restore content", data)
	}
}

func TestRestoreRecreatesDeletedInstance(t *testing.T) {
	mgr, stateStore, root, lifecycle := newTestEnv(t)
	rec, err := mgr.Create(context.Background(), "ts1", false)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := stateStore.Update(func(st *store.State) error {
		st.Instances = []model.Instance{}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(root, "ts1")); err != nil {
		t.Fatal(err)
	}
	lifecycle.running = false

	if _, err := mgr.Restore(context.Background(), "ts1", rec.File); err != nil {
		t.Fatalf("restore deleted instance: %v", err)
	}
	st, _ := stateStore.Snapshot()
	if len(st.Instances) != 1 {
		t.Fatalf("instances = %+v, want the restored instance", st.Instances)
	}
	inst := st.Instances[0]
	if inst.ID != "ts1" || inst.Ports != testInstance().Ports || inst.Subdomain != "ts1.example.com" {
		t.Fatalf("restored instance = %+v", inst)
	}
	data, err := os.ReadFile(filepath.Join(root, "ts1", "ts3server.sqlitedb"))
	if err != nil || string(data) != "sqlite-identity-v1" {
		t.Fatalf("identity = %q err=%v", data, err)
	}
	if !lifecycle.running {
		t.Fatal("recreated instance not started")
	}
}

func TestRestoreRejectsUnknownArchive(t *testing.T) {
	mgr, _, _, _ := newTestEnv(t)
	if _, err := mgr.Restore(context.Background(), "ts1", "../../etc/passwd"); !errors.Is(err, ErrBackupNotFound) {
		t.Fatalf("err = %v, want ErrBackupNotFound", err)
	}
	if _, _, err := mgr.Open("ts1", "ts1-20260101-000000.tar.gz"); !errors.Is(err, ErrBackupNotFound) {
		t.Fatalf("open err = %v, want ErrBackupNotFound", err)
	}
}

func TestPruneKeepsRetentionAndManualBackups(t *testing.T) {
	mgr, stateStore, root, _ := newTestEnv(t)
	base := time.Date(2026, 10, 22, 4, 0, 0, 0, time.UTC)
	current := base
	mgr.now = func() time.Time { return current }

	var autoFiles []string
	for i := 0; i < 3; i++ {
		current = base.Add(time.Duration(i) * time.Minute)
		rec, err := mgr.Create(context.Background(), "ts1", true)
		if err != nil {
			t.Fatalf("auto backup %d: %v", i, err)
		}
		autoFiles = append(autoFiles, rec.File)
	}
	current = base.Add(10 * time.Minute)
	manual, err := mgr.Create(context.Background(), "ts1", false)
	if err != nil {
		t.Fatalf("manual backup: %v", err)
	}

	removed, err := mgr.Prune(context.Background(), "ts1", 2)
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if removed != 1 {
		t.Fatalf("removed = %d, want 1", removed)
	}
	for i, name := range autoFiles {
		_, statErr := os.Stat(filepath.Join(root, ".backups", name))
		wantGone := i == 0
		if wantGone && !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("old auto backup %s still exists", name)
		}
		if !wantGone && statErr != nil {
			t.Fatalf("kept auto backup %s missing: %v", name, statErr)
		}
	}
	if _, err := os.Stat(filepath.Join(root, ".backups", manual.File)); err != nil {
		t.Fatalf("manual backup was pruned: %v", err)
	}
	st, _ := stateStore.Snapshot()
	if len(st.Backups) != 3 {
		t.Fatalf("state backups = %d, want 3", len(st.Backups))
	}
}

func TestArchiveNameIsUniqueWithinASecond(t *testing.T) {
	at := time.Date(2026, 10, 22, 4, 30, 0, 0, time.UTC)
	seen := map[string]bool{}
	first := archiveName("ts1", at, func(name string) bool { return seen[name] })
	seen[first] = true
	second := archiveName("ts1", at, func(name string) bool { return seen[name] })
	if first == second {
		t.Fatalf("duplicate name %q", first)
	}
	if !strings.HasPrefix(second, strings.TrimSuffix(first, ".tar.gz")+"-") {
		t.Fatalf("unexpected second name %q", second)
	}
}

func craftArchive(t *testing.T, entries map[string]string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "crafted.tar.gz")
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, content := range entries {
		hdr := &tar.Header{Name: name, Mode: 0o644, Size: int64(len(content)), Typeflag: tar.TypeReg}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestInspectArchiveRejectsUnsafeEntries(t *testing.T) {
	cases := []map[string]string{
		{"ts3server.sqlitedb": "ok", "../escape.txt": "x"},
		{"/absolute": "x", "ts3server.sqlitedb": "ok"},
		{"ts3server.sqlitedb": "ok", "a\\..\\b": "x"},
	}
	for _, entries := range cases {
		path := craftArchive(t, entries)
		if _, err := inspectArchive(path); !errors.Is(err, ErrArchiveUnsafe) {
			t.Fatalf("entries %v: err = %v, want ErrArchiveUnsafe", entries, err)
		}
	}
}

func TestInspectArchiveRequiresDatabase(t *testing.T) {
	path := craftArchive(t, map[string]string{"files/a.bin": "x"})
	if _, err := inspectArchive(path); !errors.Is(err, ErrArchiveNoDatabase) {
		t.Fatalf("err = %v, want ErrArchiveNoDatabase", err)
	}
}

func TestExtractArchiveRejectsTraversal(t *testing.T) {
	path := craftArchive(t, map[string]string{"ts3server.sqlitedb": "ok", "../../escape": "x"})
	dst := filepath.Join(t.TempDir(), "out")
	if err := os.MkdirAll(dst, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := extractArchive(context.Background(), path, dst); !errors.Is(err, ErrArchiveUnsafe) {
		t.Fatalf("err = %v, want ErrArchiveUnsafe", err)
	}
}
