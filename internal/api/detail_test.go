package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"ts3panel/internal/model"
	"ts3panel/internal/orchestrator"
	"ts3panel/internal/store"
)

// detailRuntime is a minimal orchestrator.Runtime for the detail handler test.
// It never talks to Docker; stats return two fake readings so the CPU delta is
// deterministic.
type detailRuntime struct {
	mu         sync.Mutex
	info       orchestrator.ContainerInfo
	logs       string
	stats      []orchestrator.StatsSample
	statsCalls int
}

func (d *detailRuntime) Ping(context.Context) error { return nil }
func (d *detailRuntime) EnsureImage(context.Context, string) error {
	return nil
}
func (d *detailRuntime) Create(context.Context, orchestrator.ContainerSpec) (string, error) {
	return "", nil
}
func (d *detailRuntime) Start(context.Context, string) error { return nil }
func (d *detailRuntime) Stop(context.Context, string, time.Duration) error {
	return nil
}
func (d *detailRuntime) Restart(context.Context, string, time.Duration) error {
	return nil
}
func (d *detailRuntime) Remove(context.Context, string, bool) error { return nil }
func (d *detailRuntime) Inspect(context.Context, string) (*orchestrator.ContainerInfo, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	info := d.info
	info.Labels = map[string]string{}
	for key, value := range d.info.Labels {
		info.Labels[key] = value
	}
	return &info, nil
}
func (d *detailRuntime) Logs(context.Context, string, int) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.logs, nil
}
func (d *detailRuntime) Stats(context.Context, string) (orchestrator.StatsSample, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.statsCalls++
	if len(d.stats) == 0 {
		return orchestrator.StatsSample{}, context.DeadlineExceeded
	}
	index := d.statsCalls - 1
	if index >= len(d.stats) {
		index = len(d.stats) - 1
	}
	return d.stats[index], nil
}
func (d *detailRuntime) Close() error { return nil }

func newDetailTestEnv(t *testing.T) (*API, *store.StateStore, *detailRuntime) {
	t.Helper()
	dir := t.TempDir()
	configStore, _, err := store.LoadConfig(filepath.Join(dir, "config.toml"))
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	cfg := configStore.Snapshot()
	cfg.Providers = []store.ProviderConfig{{
		ID: "manual-main", Type: "manual", Label: "手动解析", Zone: "example.com", Enabled: true, Default: true,
	}}
	cfg.Cloudflare.PublicIP = "1.2.3.4"
	if err := configStore.Save(cfg); err != nil {
		t.Fatalf("save config: %v", err)
	}

	stateStore, err := store.OpenState(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatalf("open state: %v", err)
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(testAdminPassword), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	instance := model.Instance{
		ID: "ts1", Name: "一队", Subdomain: "ts1.example.com", ProviderID: "manual-main",
		Ports:     model.Ports{Voice: 9987, FileTransfer: 30033, Query: 10011, QuerySSH: 10022},
		Container: "ts3-ts1", Status: model.InstanceStatusReady, DNSStatus: model.DNSStatusNone,
		AdminToken: "AdminToken123456", CreatedAt: time.Now().UTC(),
	}
	if err := stateStore.Update(func(st *store.State) error {
		st.SetupCompleted = true
		st.User = &model.User{Username: "admin", PasswordHash: string(hash), CreatedAt: time.Now().UTC()}
		st.Instances = []model.Instance{instance}
		return nil
	}); err != nil {
		t.Fatalf("seed state: %v", err)
	}

	root := filepath.Join(dir, "instances")
	instanceDir := filepath.Join(root, "ts1")
	if err := os.MkdirAll(filepath.Join(instanceDir, "files"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(instanceDir, "files", "data.bin"), make([]byte, 2048), 0o600); err != nil {
		t.Fatal(err)
	}

	rt := &detailRuntime{
		info: orchestrator.ContainerInfo{
			Name: "ts3-ts1", State: "running",
			Labels: map[string]string{"ts3panel.managed": "true", "ts3panel.instance": "ts1"},
		},
		logs: "starting\ntoken=AdminToken123456\nserveradmin_password=SuperSecret\n",
		stats: []orchestrator.StatsSample{
			{TotalCPU: 100, SystemCPU: 1000, OnlineCPUs: 2, ReadAt: time.Now()},
			{TotalCPU: 300, SystemCPU: 3000, OnlineCPUs: 2, MemoryUsed: 128 << 20, MemoryLimit: 512 << 20, ReadAt: time.Now()},
		},
	}
	manager, err := orchestrator.New(orchestrator.Options{
		Store:          stateStore,
		Root:           root,
		Runtime:        rt,
		MaxInstances:   20,
		StatsSampleGap: time.Millisecond,
	})
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	a, err := New(Options{Config: configStore, State: stateStore, Version: "test", Manager: manager})
	if err != nil {
		t.Fatalf("new api: %v", err)
	}
	return a, stateStore, rt
}

func TestGetInstanceDetail(t *testing.T) {
	a, _, _ := newDetailTestEnv(t)

	req := httptest.NewRequest(http.MethodGet, "/api/instances/ts1", nil)
	req.SetPathValue("id", "ts1")
	rec := httptest.NewRecorder()
	a.handleGetInstance(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}

	var detail instanceDetailResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if detail.AdminToken != "AdminToken123456" || !detail.TokenAvailable {
		t.Fatalf("token = %q available=%v", detail.AdminToken, detail.TokenAvailable)
	}
	if !detail.DNSManual || detail.Provider == nil || detail.Provider.Zone != "example.com" {
		t.Fatalf("provider metadata = %+v manual=%v", detail.Provider, detail.DNSManual)
	}
	if detail.PublicIP != "1.2.3.4" {
		t.Fatalf("public_ip = %q", detail.PublicIP)
	}
	if !detail.LogsAvailable || detail.LogsLines != 3 {
		t.Fatalf("logs available=%v lines=%d", detail.LogsAvailable, detail.LogsLines)
	}
	if strings.Contains(detail.Logs, "AdminToken123456") || strings.Contains(detail.Logs, "SuperSecret") {
		t.Fatalf("detail logs leaked a secret: %q", detail.Logs)
	}
	if detail.DataSizeBytes != 2048 {
		t.Fatalf("data size = %d, want 2048", detail.DataSizeBytes)
	}
	if detail.Stats.CPUPercent != 20 || detail.Stats.MemoryBytes != 128<<20 {
		t.Fatalf("stats = %+v", detail.Stats)
	}
	if detail.ContainerState != "running" {
		t.Fatalf("container state = %q", detail.ContainerState)
	}
}

func TestGetInstanceDetailNotFound(t *testing.T) {
	a, _, _ := newDetailTestEnv(t)
	req := httptest.NewRequest(http.MethodGet, "/api/instances/nope", nil)
	req.SetPathValue("id", "nope")
	rec := httptest.NewRecorder()
	a.handleGetInstance(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (body=%s)", rec.Code, rec.Body.String())
	}
}
