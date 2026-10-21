package api

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ts3panel/internal/model"
	"ts3panel/internal/store"
)

func makeTarGz(t *testing.T, entries map[string]string) []byte {
	t.Helper()
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
	return buf.Bytes()
}

func seedBackup(t *testing.T, a *API, st *store.StateStore, file, dbContent string) {
	t.Helper()
	if err := os.MkdirAll(a.backups.BackupDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	data := makeTarGz(t, map[string]string{"ts3server.sqlitedb": dbContent})
	if err := os.WriteFile(filepath.Join(a.backups.BackupDir(), file), data, 0o600); err != nil {
		t.Fatal(err)
	}
	rec := model.BackupRecord{
		InstanceID: "ts1",
		File:       file,
		SizeBytes:  int64(len(data)),
		CreatedAt:  time.Now().UTC(),
		Name:       "一队",
		Subdomain:  "ts1.example.com",
		Container:  "ts3-ts1",
		Ports:      model.Ports{Voice: 9987, FileTransfer: 30033, Query: 10011, QuerySSH: 10022},
	}
	if err := st.Update(func(state *store.State) error {
		state.Backups = append(state.Backups, rec)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestBackupListAndDownload(t *testing.T) {
	a, st, _ := newDetailTestEnv(t)
	t.Cleanup(func() { _ = a.Close() })
	seedBackup(t, a, st, "ts1-20261022-043000.tar.gz", "identity-v1")

	req := httptest.NewRequest(http.MethodGet, "/api/instances/ts1/backups", nil)
	req.SetPathValue("id", "ts1")
	rec := httptest.NewRecorder()
	a.handleListBackups(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var list []backupView
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(list) != 1 || !list[0].Exists || list[0].File != "ts1-20261022-043000.tar.gz" {
		t.Fatalf("list = %+v", list)
	}

	dl := httptest.NewRequest(http.MethodGet, "/api/instances/ts1/backup?file=ts1-20261022-043000.tar.gz", nil)
	dl.SetPathValue("id", "ts1")
	dlRec := httptest.NewRecorder()
	a.handleDownloadBackup(dlRec, dl)
	if dlRec.Code != http.StatusOK {
		t.Fatalf("download status = %d body=%s", dlRec.Code, dlRec.Body.String())
	}
	if !strings.Contains(dlRec.Header().Get("Content-Disposition"), "ts1-20261022-043000.tar.gz") {
		t.Fatalf("content-disposition = %q", dlRec.Header().Get("Content-Disposition"))
	}
	if dlRec.Body.Len() == 0 {
		t.Fatal("download body is empty")
	}

	missing := httptest.NewRequest(http.MethodGet, "/api/instances/ts1/backup?file=ts1-20260101-000000.tar.gz", nil)
	missing.SetPathValue("id", "ts1")
	missingRec := httptest.NewRecorder()
	a.handleDownloadBackup(missingRec, missing)
	if missingRec.Code != http.StatusNotFound {
		t.Fatalf("missing status = %d, want 404", missingRec.Code)
	}
	var errBody struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(missingRec.Body.Bytes(), &errBody); err != nil || errBody.Error.Code != "E_BACKUP_NOT_FOUND" {
		t.Fatalf("error body = %s", missingRec.Body.String())
	}
}

func TestRestoreEndpoint(t *testing.T) {
	a, st, _ := newDetailTestEnv(t)
	t.Cleanup(func() { _ = a.Close() })
	seedBackup(t, a, st, "ts1-20261022-043000.tar.gz", "identity-v1")

	req := httptest.NewRequest(http.MethodPost, "/api/instances/ts1/restore",
		strings.NewReader(`{"file":"ts1-20261022-043000.tar.gz"}`))
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("id", "ts1")
	rec := httptest.NewRecorder()
	a.handleRestore(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("restore status = %d body=%s", rec.Code, rec.Body.String())
	}
	data, err := os.ReadFile(filepath.Join(a.instances.Root(), "ts1", "ts3server.sqlitedb"))
	if err != nil {
		t.Fatalf("restored database: %v", err)
	}
	if string(data) != "identity-v1" {
		t.Fatalf("restored database = %q", data)
	}
}

func TestRestoreEndpointRejectsInvalidArchive(t *testing.T) {
	a, st, _ := newDetailTestEnv(t)
	t.Cleanup(func() { _ = a.Close() })
	seedBackup(t, a, st, "ts1-20261022-043000.tar.gz", "identity-v1")
	// Replace the archive with one that has no sqlite database.
	if err := os.WriteFile(filepath.Join(a.backups.BackupDir(), "ts1-20261022-043000.tar.gz"),
		makeTarGz(t, map[string]string{"files/a.bin": "x"}), 0o600); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/instances/ts1/restore",
		strings.NewReader(`{"file":"ts1-20261022-043000.tar.gz"}`))
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("id", "ts1")
	rec := httptest.NewRecorder()
	a.handleRestore(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "E_BACKUP_INVALID") {
		t.Fatalf("body = %s", rec.Body.String())
	}
}

func TestBackupPlanGetAndUpdate(t *testing.T) {
	a, st, _ := newDetailTestEnv(t)
	t.Cleanup(func() { _ = a.Close() })

	get := httptest.NewRequest(http.MethodGet, "/api/backup/plan", nil)
	getRec := httptest.NewRecorder()
	a.handleGetBackupPlan(getRec, get)
	if getRec.Code != http.StatusOK {
		t.Fatalf("get status = %d", getRec.Code)
	}
	var plan backupPlanView
	if err := json.Unmarshal(getRec.Body.Bytes(), &plan); err != nil {
		t.Fatal(err)
	}
	if plan.Enabled || plan.Mode != "daily" || plan.Retention != 7 {
		t.Fatalf("initial plan = %+v", plan)
	}

	body := `{"enabled":true,"mode":"interval","interval_hours":6,"retention":3}`
	put := httptest.NewRequest(http.MethodPut, "/api/backup/plan", strings.NewReader(body))
	put.Header.Set("Content-Type", "application/json")
	putRec := httptest.NewRecorder()
	a.handleUpdateBackupPlan(putRec, put)
	if putRec.Code != http.StatusOK {
		t.Fatalf("put status = %d body=%s", putRec.Code, putRec.Body.String())
	}
	if err := json.Unmarshal(putRec.Body.Bytes(), &plan); err != nil {
		t.Fatal(err)
	}
	if !plan.Enabled || plan.Mode != "interval" || plan.IntervalHours != 6 || plan.Retention != 3 {
		t.Fatalf("updated plan = %+v", plan)
	}
	if plan.NextRunAt == nil {
		t.Fatal("next_run_at is empty after enabling the plan")
	}
	saved := a.config.Snapshot()
	if !saved.Backup.Enabled || saved.Backup.IntervalHours != 6 || saved.Backup.Retention != 3 {
		t.Fatalf("config not saved: %+v", saved.Backup)
	}
	state, _ := st.Snapshot()
	if state.Backup.NextRunAt == nil {
		t.Fatal("next run not persisted in state.json")
	}
}

// probeCounter is a deterministic voicePortProbe for handler tests.
type probeCounter struct {
	calls  int
	result portProbeResult
}

func (p *probeCounter) Check(context.Context, string, int) portProbeResult {
	p.calls++
	return p.result
}

func TestPortsCheckDisabledByDefault(t *testing.T) {
	instances := []model.Instance{portTestInstance("ts1", 9987, 30033, 10011, 10022)}
	a := newPortsTestAPI(t, instances, "1.2.3.4")
	probe := &probeCounter{result: portProbeResult{State: portCheckOpen}}
	a.voiceProbe = probe
	a.probeCache = newVoiceProbeCache()

	rec := httptest.NewRecorder()
	a.handlePortsCheck(rec, httptest.NewRequest(http.MethodGet, "/api/ports/check", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var body portsCheckResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Enabled {
		t.Fatal("external check is enabled by default")
	}
	if probe.calls != 0 {
		t.Fatalf("third-party probe called %d times while disabled", probe.calls)
	}
	if len(body.Results) != 1 || body.Results[0].State != portCheckUnknown {
		t.Fatalf("results = %+v, want one unknown row", body.Results)
	}
	if !strings.Contains(body.Note, "第三方") {
		t.Fatalf("note = %q", body.Note)
	}
}

func TestPortsCheckEnabledAndCached(t *testing.T) {
	instances := []model.Instance{portTestInstance("ts1", 9987, 30033, 10011, 10022)}
	a := newPortsTestAPI(t, instances, "1.2.3.4")
	if err := a.config.Update(func(cfg *store.Config) error {
		cfg.Ports.ExternalCheck = true
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	probe := &probeCounter{result: portProbeResult{State: portCheckOpen, Detail: "ok"}}
	a.voiceProbe = probe
	a.probeCache = newVoiceProbeCache()

	for i := 0; i < 2; i++ {
		rec := httptest.NewRecorder()
		a.handlePortsCheck(rec, httptest.NewRequest(http.MethodGet, "/api/ports/check", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d", rec.Code)
		}
		var body portsCheckResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if !body.Enabled || len(body.Results) != 1 || body.Results[0].State != portCheckOpen {
			t.Fatalf("body = %+v", body)
		}
		if body.CacheSeconds != 60 {
			t.Fatalf("cache_seconds = %d, want 60", body.CacheSeconds)
		}
	}
	if probe.calls != 1 {
		t.Fatalf("probe calls = %d, want 1 (second request served from cache)", probe.calls)
	}
}

func TestRestoreUploadMultipart(t *testing.T) {
	a, _, _ := newDetailTestEnv(t)
	t.Cleanup(func() { _ = a.Close() })

	archive := makeTarGz(t, map[string]string{"ts3server.sqlitedb": "uploaded-identity"})
	var body bytes.Buffer
	body.WriteString("--BOUND\r\n")
	body.WriteString("Content-Disposition: form-data; name=\"file\"; filename=\"upload.tar.gz\"\r\n")
	body.WriteString("Content-Type: application/gzip\r\n\r\n")
	body.Write(archive)
	body.WriteString("\r\n--BOUND--\r\n")

	req := httptest.NewRequest(http.MethodPost, "/api/instances/ts1/restore", &body)
	req.Header.Set("Content-Type", "multipart/form-data; boundary=BOUND")
	req.SetPathValue("id", "ts1")
	rec := httptest.NewRecorder()
	a.handleRestore(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	data, err := os.ReadFile(filepath.Join(a.instances.Root(), "ts1", "ts3server.sqlitedb"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "uploaded-identity" {
		t.Fatalf("restored data = %q", data)
	}
	// The uploaded archive must have been stored as a backup record.
	recs, err := a.backups.List("ts1")
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 1 {
		t.Fatalf("backup records = %+v, want the imported upload", recs)
	}
}
