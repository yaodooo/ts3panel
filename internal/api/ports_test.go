package api

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"ts3panel/internal/model"
	"ts3panel/internal/store"
)

func portTestInstance(id string, voice, filetransfer, query, querySSH int) model.Instance {
	return model.Instance{
		ID:        id,
		Name:      "实例 " + id,
		Container: "ts3-" + id,
		Status:    model.InstanceStatusReady,
		Ports: model.Ports{
			Voice:        voice,
			FileTransfer: filetransfer,
			Query:        query,
			QuerySSH:     querySSH,
		},
	}
}

func newPortsTestAPI(t *testing.T, instances []model.Instance, publicIP string) *API {
	t.Helper()
	dir := t.TempDir()
	cfgStore, _, err := store.LoadConfig(filepath.Join(dir, "config.toml"))
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	cfg := cfgStore.Snapshot()
	cfg.Cloudflare.PublicIP = publicIP
	if err := cfgStore.Save(cfg); err != nil {
		t.Fatalf("save config: %v", err)
	}

	stateStore, err := store.OpenState(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatalf("open state: %v", err)
	}
	if err := stateStore.Update(func(st *store.State) error {
		st.SetupCompleted = true
		st.User = &model.User{Username: "admin", PasswordHash: "not-used", CreatedAt: time.Now().UTC()}
		st.Instances = instances
		return nil
	}); err != nil {
		t.Fatalf("seed state: %v", err)
	}
	return &API{config: cfgStore, state: stateStore}
}

func decodePortsResponse(t *testing.T, rec *httptest.ResponseRecorder) portsResponse {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var body portsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return body
}

func TestCollectPortRangesMergesContiguousPorts(t *testing.T) {
	instances := []model.Instance{
		portTestInstance("ts1", 9987, 30033, 10011, 10022),
		portTestInstance("ts2", 9988, 30034, 10012, 10023),
	}
	public, internal := collectPortRanges(instances)

	if len(public) != 2 {
		t.Fatalf("public ranges = %+v, want 2", public)
	}
	if public[0].Display != "UDP 9987-9988" || public[0].Purpose != portPurposeVoice {
		t.Fatalf("first public range = %+v", public[0])
	}
	if public[1].Display != "TCP 30033-30034" || public[1].Purpose != portPurposeFileTransfer {
		t.Fatalf("second public range = %+v", public[1])
	}
	if len(internal) != 2 {
		t.Fatalf("internal ranges = %+v, want 2", internal)
	}
	if internal[0].Display != "TCP 10011-10012" || internal[0].Purpose != portPurposeQuery {
		t.Fatalf("query range = %+v", internal[0])
	}
	if internal[1].Display != "TCP 10022-10023" || internal[1].Purpose != portPurposeQuerySSH {
		t.Fatalf("ssh query range = %+v", internal[1])
	}
}

func TestCollectPortRangesKeepsGapsAndSorts(t *testing.T) {
	instances := []model.Instance{
		portTestInstance("ts3", 9989, 30035, 10013, 10024),
		portTestInstance("ts1", 9987, 30033, 10011, 10022),
	}
	public, _ := collectPortRanges(instances)
	if len(public) != 4 {
		t.Fatalf("public ranges = %+v, want 4 separate ranges", public)
	}
	if public[0].Display != "UDP 9987" || public[1].Display != "UDP 9989" {
		t.Fatalf("UDP ranges = %+v", public[:2])
	}
	if public[2].Display != "TCP 30033" || public[3].Display != "TCP 30035" {
		t.Fatalf("TCP ranges = %+v", public[2:])
	}
}

func TestCollectPortRangesDeduplicates(t *testing.T) {
	instances := []model.Instance{
		portTestInstance("ts1", 9987, 30033, 10011, 10022),
		portTestInstance("dup", 9987, 30033, 10011, 10022),
	}
	public, internal := collectPortRanges(instances)
	if len(public) != 2 || len(internal) != 2 {
		t.Fatalf("public=%+v internal=%+v, want deduplicated ranges", public, internal)
	}
}

func TestRenderPortsText(t *testing.T) {
	instances := []model.Instance{
		portTestInstance("ts1", 9987, 30033, 10011, 10022),
		portTestInstance("ts2", 9988, 30034, 10012, 10023),
	}
	public, internal := collectPortRanges(instances)
	text := renderPortsText(public, internal)

	for _, want := range []string{
		"需要开放的安全组入方向：",
		"UDP 9987-9988",
		"语音（全部实例）",
		"TCP 30033-30034",
		"文件传输（全部实例）",
		"不要开放：TCP 10011-10012、10022-10023（查询端口，仅监听 127.0.0.1）",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("text missing %q:\n%s", want, text)
		}
	}
}

func TestRenderPortsTextWithoutInstances(t *testing.T) {
	text := renderPortsText(nil, nil)
	if !strings.Contains(text, "当前没有实例，无需开放端口。") {
		t.Fatalf("text = %q", text)
	}
}

func TestHandlePorts(t *testing.T) {
	a := newPortsTestAPI(t, []model.Instance{
		portTestInstance("ts1", 9987, 30033, 10011, 10022),
	}, "")
	rec := httptest.NewRecorder()
	a.handlePorts(rec, httptest.NewRequest(http.MethodGet, "/api/ports", nil))

	body := decodePortsResponse(t, rec)
	if body.InstanceCount != 1 {
		t.Fatalf("instance_count = %d, want 1", body.InstanceCount)
	}
	if len(body.Public) != 2 || len(body.DoNotOpen) != 2 {
		t.Fatalf("body = %+v", body)
	}
	if !strings.Contains(body.Text, "UDP 9987") {
		t.Fatalf("text = %q", body.Text)
	}
}

type fakePortProbe struct {
	mu    sync.Mutex
	calls []probeCall
	fn    func(publicIP string, port int) portProbeResult
}

type probeCall struct {
	publicIP string
	port     int
}

func (f *fakePortProbe) Check(_ context.Context, publicIP string, port int) portProbeResult {
	f.mu.Lock()
	f.calls = append(f.calls, probeCall{publicIP: publicIP, port: port})
	f.mu.Unlock()
	if f.fn != nil {
		return f.fn(publicIP, port)
	}
	return portProbeResult{State: portCheckUnknown, Detail: "fake"}
}

func (f *fakePortProbe) callsSnapshot() []probeCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]probeCall, len(f.calls))
	copy(out, f.calls)
	return out
}

func enableExternalCheck(t *testing.T, a *API) {
	t.Helper()
	if err := a.config.Update(func(cfg *store.Config) error {
		cfg.Ports.ExternalCheck = true
		return nil
	}); err != nil {
		t.Fatalf("enable external check: %v", err)
	}
}

func TestHandlePortsCheckUsesProbeAndSorts(t *testing.T) {
	a := newPortsTestAPI(t, []model.Instance{
		portTestInstance("ts2", 9988, 30034, 10012, 10023),
		portTestInstance("ts1", 9987, 30033, 10011, 10022),
	}, "1.2.3.4")
	enableExternalCheck(t, a)
	probe := &fakePortProbe{fn: func(_ string, port int) portProbeResult {
		if port == 9988 {
			return portProbeResult{State: portCheckUnknown, Detail: "探测服务超时"}
		}
		return portProbeResult{State: portCheckOpen, Detail: "收到响应"}
	}}
	a.voiceProbe = probe

	rec := httptest.NewRecorder()
	a.handlePortsCheck(rec, httptest.NewRequest(http.MethodGet, "/api/ports/check", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var body portsCheckResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Results) != 2 {
		t.Fatalf("results = %+v", body.Results)
	}
	if body.Results[0].Port != 9987 || body.Results[0].State != portCheckOpen {
		t.Fatalf("first result = %+v", body.Results[0])
	}
	if body.Results[1].Port != 9988 || body.Results[1].State != portCheckUnknown {
		t.Fatalf("second result = %+v", body.Results[1])
	}
	if body.Results[0].InstanceName != "实例 ts1" || body.Results[0].Protocol != portProtocolUDP {
		t.Fatalf("metadata missing: %+v", body.Results[0])
	}
	for _, call := range probe.callsSnapshot() {
		if call.publicIP != "1.2.3.4" {
			t.Fatalf("probe public ip = %q", call.publicIP)
		}
	}
}

func TestHandlePortsCheckWithoutPublicIP(t *testing.T) {
	a := newPortsTestAPI(t, []model.Instance{
		portTestInstance("ts1", 9987, 30033, 10011, 10022),
	}, "")
	enableExternalCheck(t, a)
	var observedIP string
	var observedPort int
	probe := &fakePortProbe{fn: func(ip string, port int) portProbeResult {
		observedIP = ip
		observedPort = port
		return portProbeResult{State: portCheckUnknown, Detail: "未配置公网 IP"}
	}}
	a.voiceProbe = probe

	rec := httptest.NewRecorder()
	a.handlePortsCheck(rec, httptest.NewRequest(http.MethodGet, "/api/ports/check", nil))
	var body portsCheckResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if observedIP != "" || observedPort != 9987 {
		t.Fatalf("probe called with ip=%q port=%d", observedIP, observedPort)
	}
	if !strings.Contains(body.Note, "未配置公网 IP") {
		t.Fatalf("note = %q", body.Note)
	}
	if body.Results[0].State != portCheckUnknown {
		t.Fatalf("state = %q, want unknown", body.Results[0].State)
	}
}

func TestHandlePortsCheckWithoutInstances(t *testing.T) {
	a := newPortsTestAPI(t, nil, "1.2.3.4")
	rec := httptest.NewRecorder()
	a.handlePortsCheck(rec, httptest.NewRequest(http.MethodGet, "/api/ports/check", nil))
	var body portsCheckResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Results) != 0 || body.Results == nil {
		t.Fatalf("results = %#v, want empty slice", body.Results)
	}
	if !strings.Contains(body.Note, "当前没有实例") {
		t.Fatalf("note = %q", body.Note)
	}
}

func TestInterpretCheckHostResults(t *testing.T) {
	cases := []struct {
		name      string
		raw       map[string]any
		wantState string
		wantDone  bool
	}{
		{
			name:      "open",
			raw:       map[string]any{"node": []any{map[string]any{"time": 0.03}}},
			wantState: portCheckOpen,
			wantDone:  true,
		},
		{
			name:      "refused is closed",
			raw:       map[string]any{"node": []any{map[string]any{"error": "Connection refused"}}},
			wantState: portCheckClosed,
			wantDone:  true,
		},
		{
			name:      "timeout is unknown not closed",
			raw:       map[string]any{"node": []any{map[string]any{"error": "Connection timed out"}}},
			wantState: portCheckUnknown,
			wantDone:  true,
		},
		{
			name:      "host unreachable is unknown not closed",
			raw:       map[string]any{"node": []any{map[string]any{"error": "No route to host"}}},
			wantState: portCheckUnknown,
			wantDone:  true,
		},
		{
			name:     "pending null",
			raw:      map[string]any{"node": nil},
			wantDone: false,
		},
		{
			name:     "pending empty array",
			raw:      map[string]any{"node": []any{}},
			wantDone: false,
		},
		{
			name:     "empty document",
			raw:      map[string]any{},
			wantDone: false,
		},
		{
			name:      "nested array",
			raw:       map[string]any{"node": []any{[]any{map[string]any{"time": 0.1}}}},
			wantState: portCheckOpen,
			wantDone:  true,
		},
		{
			name:      "string open",
			raw:       map[string]any{"node": "OK"},
			wantState: portCheckOpen,
			wantDone:  true,
		},
		{
			name:      "string timeout",
			raw:       map[string]any{"node": "TIMEOUT"},
			wantState: portCheckUnknown,
			wantDone:  true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			state, _, done := interpretCheckHostResults(tc.raw)
			if done != tc.wantDone {
				t.Fatalf("done = %v, want %v", done, tc.wantDone)
			}
			if !done {
				return
			}
			if state != tc.wantState {
				t.Fatalf("state = %q, want %q", state, tc.wantState)
			}
		})
	}
}

func TestUDPPortInUse(t *testing.T) {
	conn, err := net.ListenPacket("udp4", "0.0.0.0:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := conn.LocalAddr().(*net.UDPAddr).Port
	if !udpPortInUse(port) {
		t.Fatalf("udpPortInUse(%d) = false while a listener is open", port)
	}
	if err := conn.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if udpPortInUse(port) {
		t.Fatalf("udpPortInUse(%d) = true after close", port)
	}
}

func testVoiceProbe() *checkHostVoiceProbe {
	return &checkHostVoiceProbe{
		client:    &http.Client{Timeout: 200 * time.Millisecond},
		baseURL:   "http://127.0.0.1:1",
		pollEvery: 10 * time.Millisecond,
		wait:      50 * time.Millisecond,
	}
}

func TestVoiceProbeClosedWithoutLocalListener(t *testing.T) {
	conn, err := net.ListenPacket("udp4", "0.0.0.0:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := conn.LocalAddr().(*net.UDPAddr).Port
	if err := conn.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	result := testVoiceProbe().Check(context.Background(), "203.0.113.10", port)
	if result.State != portCheckClosed {
		t.Fatalf("state = %q, want closed (detail=%q)", result.State, result.Detail)
	}
}

func TestVoiceProbeUnknownWhenExternalProbeFails(t *testing.T) {
	conn, err := net.ListenPacket("udp4", "0.0.0.0:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer conn.Close()
	port := conn.LocalAddr().(*net.UDPAddr).Port

	result := testVoiceProbe().Check(context.Background(), "203.0.113.10", port)
	if result.State != portCheckUnknown {
		t.Fatalf("state = %q, want unknown (detail=%q)", result.State, result.Detail)
	}
}

func TestVoiceProbeUnknownWithoutPublicIP(t *testing.T) {
	conn, err := net.ListenPacket("udp4", "0.0.0.0:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer conn.Close()
	port := conn.LocalAddr().(*net.UDPAddr).Port

	result := testVoiceProbe().Check(context.Background(), "", port)
	if result.State != portCheckUnknown {
		t.Fatalf("state = %q, want unknown (detail=%q)", result.State, result.Detail)
	}
	if !strings.Contains(result.Detail, "公网 IP") {
		t.Fatalf("detail = %q", result.Detail)
	}
}

func TestVoiceProbeExternalOpen(t *testing.T) {
	conn, err := net.ListenPacket("udp4", "0.0.0.0:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer conn.Close()
	port := conn.LocalAddr().(*net.UDPAddr).Port

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/check-udp"):
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": 1, "request_id": "req-1"})
		case strings.HasPrefix(r.URL.Path, "/check-result/"):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"node": []any{map[string]any{"time": 0.02}},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	probe := &checkHostVoiceProbe{
		client:    &http.Client{Timeout: time.Second},
		baseURL:   srv.URL,
		pollEvery: time.Millisecond,
		wait:      time.Second,
	}
	result := probe.Check(context.Background(), "203.0.113.10", port)
	if result.State != portCheckOpen {
		t.Fatalf("state = %q, want open (detail=%q)", result.State, result.Detail)
	}
}

func TestVoiceProbeExternalLimit(t *testing.T) {
	conn, err := net.ListenPacket("udp4", "0.0.0.0:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer conn.Close()
	port := conn.LocalAddr().(*net.UDPAddr).Port

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"error": "limit_exceeded"})
	}))
	defer srv.Close()

	probe := &checkHostVoiceProbe{
		client:    &http.Client{Timeout: time.Second},
		baseURL:   srv.URL,
		pollEvery: time.Millisecond,
		wait:      time.Second,
	}
	result := probe.Check(context.Background(), "203.0.113.10", port)
	if result.State != portCheckUnknown {
		t.Fatalf("state = %q, want unknown", result.State)
	}
	if !strings.Contains(result.Detail, "限流") {
		t.Fatalf("detail = %q", result.Detail)
	}
}

func TestVoiceProbeExternalPendingTimesOutUnknown(t *testing.T) {
	conn, err := net.ListenPacket("udp4", "0.0.0.0:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer conn.Close()
	port := conn.LocalAddr().(*net.UDPAddr).Port

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/check-udp") {
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": 1, "request_id": "req-2"})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"node": nil})
	}))
	defer srv.Close()

	probe := &checkHostVoiceProbe{
		client:    &http.Client{Timeout: time.Second},
		baseURL:   srv.URL,
		pollEvery: 5 * time.Millisecond,
		wait:      20 * time.Millisecond,
	}
	result := probe.Check(context.Background(), "203.0.113.10", port)
	if result.State != portCheckUnknown {
		t.Fatalf("state = %q, want unknown", result.State)
	}
}

func TestPortsRouteRequiresAuth(t *testing.T) {
	apiHandler, _ := newAuthTestAPI(t)
	handler := apiHandler.Handler()

	unauthorized := httptest.NewRecorder()
	handler.ServeHTTP(unauthorized, httptest.NewRequest(http.MethodGet, "/api/ports", nil))
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status = %d, want 401", unauthorized.Code)
	}

	login := httptest.NewRequest(http.MethodPost, "/api/login",
		strings.NewReader(`{"username":"admin","password":"AdminPassw0rd1"}`))
	login.Header.Set("Content-Type", "application/json")
	login.Header.Set("X-Requested-With", "ts3panel")
	loginRec := httptest.NewRecorder()
	handler.ServeHTTP(loginRec, login)
	if loginRec.Code != http.StatusOK {
		t.Fatalf("login status = %d, body=%s", loginRec.Code, loginRec.Body.String())
	}
	cookies := loginRec.Result().Cookies()
	if len(cookies) == 0 {
		t.Fatal("login did not set a session cookie")
	}

	req := httptest.NewRequest(http.MethodGet, "/api/ports", nil)
	req.AddCookie(cookies[0])
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("authenticated status = %d, body=%s", rec.Code, rec.Body.String())
	}
	body := decodePortsResponse(t, rec)
	if body.InstanceCount != 0 || len(body.Public) != 0 {
		t.Fatalf("body = %+v, want empty checklist", body)
	}
}
