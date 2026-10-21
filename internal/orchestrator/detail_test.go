package orchestrator

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ts3panel/internal/model"
)

func TestMaskSecrets(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "token assignment",
			in:   "2026-10-21 token=AbCdEfGhIjKlMnOpQrStUv starting",
			want: "2026-10-21 token=**** starting",
		},
		{
			name: "privilege key",
			in:   "  Privilege Key: AbCdEfGhIjKlMnOpQrStUv\n",
			want: "  Privilege Key: ****\n",
		},
		{
			name: "serveradmin password",
			in:   "serveradmin_password=SuperSecret123 login ok",
			want: "serveradmin_password=**** login ok",
		},
		{
			name: "password with colon and spaces",
			in:   "password:   \"two words\"",
			want: "password:   ****",
		},
		{
			name: "api token",
			in:   "api_token = a1b2c3d4",
			want: "api_token = ****",
		},
		{
			name: "already masked",
			in:   "token=****",
			want: "token=****",
		},
		{
			name: "ordinary line untouched",
			in:   "Server started, listening on 9987/udp",
			want: "Server started, listening on 9987/udp",
		},
		{
			name: "plural tokens untouched",
			in:   "tokens: 3 available",
			want: "tokens: 3 available",
		},
		{
			name: "empty value untouched",
			in:   "token=\n",
			want: "token=\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := MaskSecrets(tc.in); got != tc.want {
				t.Fatalf("MaskSecrets(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestMaskSecretsKeepsLineCount(t *testing.T) {
	in := "line one\ntoken=secret-value\nline three\nPrivilege Key: abcdefgh\n"
	out := MaskSecrets(in)
	if strings.Count(out, "\n") != strings.Count(in, "\n") {
		t.Fatalf("masking changed the line structure:\n%q", out)
	}
	if strings.Contains(out, "secret-value") || strings.Contains(out, "abcdefgh") {
		t.Fatalf("secret leaked: %q", out)
	}
}

func TestComputeResourceStats(t *testing.T) {
	first := StatsSample{TotalCPU: 100, SystemCPU: 1000, OnlineCPUs: 2, ReadAt: time.Now()}
	second := StatsSample{TotalCPU: 200, SystemCPU: 2000, OnlineCPUs: 2, MemoryUsed: 64 << 20, MemoryLimit: 512 << 20, ReadAt: time.Now()}
	stats := computeResourceStats(first, second)
	if !stats.Available || !stats.CPUAvailable {
		t.Fatalf("stats not available: %+v", stats)
	}
	if stats.CPUPercent != 20 {
		t.Fatalf("cpu = %v, want 20", stats.CPUPercent)
	}
	if stats.MemoryBytes != 64<<20 || stats.MemoryLimitBytes != 512<<20 {
		t.Fatalf("memory = %d/%d", stats.MemoryBytes, stats.MemoryLimitBytes)
	}

	// A container restart resets the counters; CPU must be unavailable instead of
	// producing a negative or absurd percentage.
	reset := StatsSample{TotalCPU: 10, SystemCPU: 20, OnlineCPUs: 2, ReadAt: time.Now()}
	if got := computeResourceStats(second, reset); got.CPUAvailable {
		t.Fatalf("CPU reported after counter reset: %+v", got)
	}
}

func TestDetailAggregatesLogsStatsAndSize(t *testing.T) {
	rt := newFakeRuntime()
	rt.logs["ts3-ts1"] = "starting\ntoken=AbCdEfGhIjKlMnOpQrStUv\nserveradmin_password=SuperSecret\n"
	rt.stats = []StatsSample{
		{TotalCPU: 100, SystemCPU: 1000, OnlineCPUs: 2, ReadAt: time.Now()},
		{TotalCPU: 300, SystemCPU: 3000, OnlineCPUs: 2, MemoryUsed: 100 << 20, MemoryLimit: 512 << 20, ReadAt: time.Now()},
	}
	m, st, root := newTestManager(t, rt)
	m.statsGap = time.Millisecond

	seedInstance(t, st, model.Instance{
		ID: "ts1", Name: "一队", Subdomain: "ts1.example.com", Container: "ts3-ts1",
		Ports: PortsFor(0), Status: model.InstanceStatusReady, DNSStatus: model.DNSStatusNone,
		AdminToken: "AbCdEfGhIjKlMnOpQrStUv",
	})
	rt.containers["ts3-ts1"] = &fakeContainer{info: ContainerInfo{
		Name: "ts3-ts1", State: "running",
		Labels: map[string]string{managedLabel: "true", instanceLabel: "ts1"},
	}}

	dir := filepath.Join(root, "ts1")
	if err := os.MkdirAll(filepath.Join(dir, "files"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "files", "blob.bin"), make([]byte, 1234), 0o600); err != nil {
		t.Fatal(err)
	}

	detail, err := m.Detail(context.Background(), "ts1")
	if err != nil {
		t.Fatalf("Detail: %v", err)
	}
	if !detail.LogsAvailable || detail.LogsLines != 3 {
		t.Fatalf("logs available=%v lines=%d", detail.LogsAvailable, detail.LogsLines)
	}
	if strings.Contains(detail.Logs, "AbCdEfGhIjKlMnOpQrStUv") || strings.Contains(detail.Logs, "SuperSecret") {
		t.Fatalf("logs leaked a secret: %q", detail.Logs)
	}
	if !strings.Contains(detail.Logs, "token=****") || !strings.Contains(detail.Logs, "serveradmin_password=****") {
		t.Fatalf("logs were not masked as expected: %q", detail.Logs)
	}
	if detail.DataSizeBytes != 1234 {
		t.Fatalf("data size = %d, want 1234", detail.DataSizeBytes)
	}
	if !detail.TokenAvailable || detail.AdminToken != "AbCdEfGhIjKlMnOpQrStUv" {
		t.Fatalf("token availability = %v token=%q", detail.TokenAvailable, detail.AdminToken)
	}
	if !detail.Stats.Available || !detail.Stats.CPUAvailable {
		t.Fatalf("stats unavailable: %+v", detail.Stats)
	}
	if detail.Stats.CPUPercent != 20 {
		t.Fatalf("cpu = %v, want 20", detail.Stats.CPUPercent)
	}
	if detail.Stats.MemoryBytes != 100<<20 {
		t.Fatalf("memory = %d", detail.Stats.MemoryBytes)
	}
}

func TestDetailStatsAreCached(t *testing.T) {
	rt := newFakeRuntime()
	rt.logs["ts3-ts1"] = "running\n"
	rt.stats = []StatsSample{
		{TotalCPU: 100, SystemCPU: 1000, OnlineCPUs: 1, ReadAt: time.Now()},
		{TotalCPU: 200, SystemCPU: 2000, OnlineCPUs: 1, ReadAt: time.Now()},
	}
	m, st, _ := newTestManager(t, rt)
	m.statsGap = time.Millisecond
	seedInstance(t, st, model.Instance{
		ID: "ts1", Name: "ts1", Subdomain: "ts1", Container: "ts3-ts1",
		Ports: PortsFor(0), Status: model.InstanceStatusReady, DNSStatus: model.DNSStatusNone,
	})
	rt.containers["ts3-ts1"] = &fakeContainer{info: ContainerInfo{
		Name: "ts3-ts1", State: "running",
		Labels: map[string]string{managedLabel: "true", instanceLabel: "ts1"},
	}}

	if _, err := m.Detail(context.Background(), "ts1"); err != nil {
		t.Fatalf("first Detail: %v", err)
	}
	if rt.statsCalls != 2 {
		t.Fatalf("stats calls = %d, want 2 (two reads for one sample)", rt.statsCalls)
	}
	if _, err := m.Detail(context.Background(), "ts1"); err != nil {
		t.Fatalf("second Detail: %v", err)
	}
	if rt.statsCalls != 2 {
		t.Fatalf("stats cache miss: calls = %d, want 2", rt.statsCalls)
	}

	m.statsTTL = time.Nanosecond
	time.Sleep(2 * time.Millisecond)
	if _, err := m.Detail(context.Background(), "ts1"); err != nil {
		t.Fatalf("third Detail: %v", err)
	}
	if rt.statsCalls != 4 {
		t.Fatalf("stats cache did not expire: calls = %d, want 4", rt.statsCalls)
	}
}

func TestDetailSurvivesMissingContainer(t *testing.T) {
	rt := newFakeRuntime()
	m, st, _ := newTestManager(t, rt)
	seedInstance(t, st, model.Instance{
		ID: "ts1", Name: "ts1", Subdomain: "ts1", Container: "ts3-ts1",
		Ports: PortsFor(0), Status: model.InstanceStatusReady, DNSStatus: model.DNSStatusNone,
	})

	detail, err := m.Detail(context.Background(), "ts1")
	if err != nil {
		t.Fatalf("Detail must not fail when the container is gone: %v", err)
	}
	if detail.Status != model.InstanceStatusMissing {
		t.Fatalf("status = %q, want missing", detail.Status)
	}
	if detail.LogsAvailable {
		t.Fatal("logs reported available for a missing container")
	}
	if detail.Stats.Available {
		t.Fatal("stats reported available for a missing container")
	}
}

func TestDetailUnknownInstance(t *testing.T) {
	rt := newFakeRuntime()
	m, _, _ := newTestManager(t, rt)
	if _, err := m.Detail(context.Background(), "nope"); err == nil {
		t.Fatal("Detail accepted an unknown instance")
	}
}

func TestDetailRefusesUnmanagedContainer(t *testing.T) {
	rt := newFakeRuntime()
	m, st, _ := newTestManager(t, rt)
	seedInstance(t, st, model.Instance{
		ID: "ts1", Name: "ts1", Subdomain: "ts1", Container: "ts3-ts1",
		Ports: PortsFor(0), Status: model.InstanceStatusReady, DNSStatus: model.DNSStatusNone,
	})
	rt.containers["ts3-ts1"] = &fakeContainer{info: ContainerInfo{
		Name: "ts3-ts1", State: "running",
		Labels: map[string]string{"someone": "else"},
	}}

	if _, err := m.Detail(context.Background(), "ts1"); !errors.Is(err, ErrNotManaged) {
		t.Fatalf("Detail err = %v, want ErrNotManaged", err)
	}
}

func TestStopInvalidatesStatsCache(t *testing.T) {
	rt := newFakeRuntime()
	rt.logs["ts3-ts1"] = "running\n"
	rt.stats = []StatsSample{
		{TotalCPU: 100, SystemCPU: 1000, OnlineCPUs: 1, ReadAt: time.Now()},
		{TotalCPU: 200, SystemCPU: 2000, OnlineCPUs: 1, ReadAt: time.Now()},
	}
	m, st, _ := newTestManager(t, rt)
	m.statsGap = time.Millisecond
	seedInstance(t, st, model.Instance{
		ID: "ts1", Name: "ts1", Subdomain: "ts1", Container: "ts3-ts1",
		Ports: PortsFor(0), Status: model.InstanceStatusReady, DNSStatus: model.DNSStatusNone,
	})
	rt.containers["ts3-ts1"] = &fakeContainer{info: ContainerInfo{
		Name: "ts3-ts1", State: "running",
		Labels: map[string]string{managedLabel: "true", instanceLabel: "ts1"},
	}}

	if _, err := m.Detail(context.Background(), "ts1"); err != nil {
		t.Fatalf("Detail: %v", err)
	}
	if rt.statsCalls != 2 {
		t.Fatalf("stats calls = %d, want 2", rt.statsCalls)
	}
	if _, err := m.Stop(context.Background(), "ts1"); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if _, err := m.Detail(context.Background(), "ts1"); err != nil {
		t.Fatalf("Detail after stop: %v", err)
	}
	if rt.statsCalls != 4 {
		t.Fatalf("stopped instance served a stale stats cache: calls = %d, want 4", rt.statsCalls)
	}
}
