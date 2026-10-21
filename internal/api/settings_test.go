package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func settingsGet(t *testing.T, a *API, query string) settingsView {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/settings"+query, nil)
	rec := httptest.NewRecorder()
	a.handleGetSettings(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET settings status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var view settingsView
	if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
		t.Fatalf("decode settings: %v", err)
	}
	return view
}

func settingsPut(t *testing.T, a *API, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPut, "/api/settings", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	a.handleUpdateSettings(rec, req)
	return rec
}

func TestSettingsPublicIPRoundTrip(t *testing.T) {
	env := newDNSTestEnv(t, dnsTestOptions{})

	if got := settingsGet(t, env.api, ""); got.PublicIP != "" {
		t.Fatalf("initial public_ip = %q, want empty", got.PublicIP)
	}

	rec := settingsPut(t, env.api, `{"public_ip":"5.6.7.8"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var saved settingsView
	if err := json.Unmarshal(rec.Body.Bytes(), &saved); err != nil {
		t.Fatalf("decode PUT: %v", err)
	}
	if saved.PublicIP != "5.6.7.8" {
		t.Fatalf("saved public_ip = %q", saved.PublicIP)
	}
	if got := env.config.Snapshot().Cloudflare.PublicIP; got != "5.6.7.8" {
		t.Fatalf("config public_ip = %q", got)
	}
	if got := settingsGet(t, env.api, ""); got.PublicIP != "5.6.7.8" {
		t.Fatalf("GET after save = %q", got.PublicIP)
	}

	// Empty string is an explicit clear, not a validation error.
	rec = settingsPut(t, env.api, `{"public_ip":""}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("clear status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if got := env.config.Snapshot().Cloudflare.PublicIP; got != "" {
		t.Fatalf("public_ip not cleared: %q", got)
	}
}

func TestSettingsRejectsInvalidPublicIP(t *testing.T) {
	env := newDNSTestEnv(t, dnsTestOptions{})
	for _, body := range []string{
		`{"public_ip":"999.1.1.1"}`,
		`{"public_ip":"not-an-ip"}`,
		`{"public_ip":"2001:db8::1"}`,
		`{}`,
	} {
		rec := settingsPut(t, env.api, body)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s: status = %d, want 400 (body=%s)", body, rec.Code, rec.Body.String())
		}
	}
}

func TestSettingsDetectIsNonBlockingOnFailure(t *testing.T) {
	env := newDNSTestEnv(t, dnsTestOptions{})
	env.api.publicIPDetector = func(context.Context) (string, error) {
		return "", errors.New("dial tcp: no route to host")
	}

	rec := settingsPut(t, env.api, `{"public_ip":"1.1.1.1"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("save status = %d", rec.Code)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/settings?detect=1", nil)
	rec = httptest.NewRecorder()
	env.api.handleGetSettings(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("detect status = %d, want 200 (failure must not block)", rec.Code)
	}
	var view settingsView
	if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if view.PublicIP != "1.1.1.1" {
		t.Fatalf("manual public_ip lost: %q", view.PublicIP)
	}
	if view.DetectedIP != "" || !strings.Contains(view.DetectError, "手动填写") {
		t.Fatalf("detect failure not reported as a warning: %+v", view)
	}
}

func TestSettingsDetectReturnsProbeValue(t *testing.T) {
	env := newDNSTestEnv(t, dnsTestOptions{publicIP: "9.9.9.9"})
	view := settingsGet(t, env.api, "?detect=1")
	if view.DetectedIP != "9.9.9.9" || view.DetectError != "" {
		t.Fatalf("detect view = %+v", view)
	}
}

func TestSettingsManualIPInvalidatesProbeCache(t *testing.T) {
	env := newDNSTestEnv(t, dnsTestOptions{publicIP: "9.9.9.9"})
	env.api.publicIPMu.Lock()
	env.api.publicIPCached = "9.9.9.9"
	env.api.publicIPMu.Unlock()

	if rec := settingsPut(t, env.api, `{"public_ip":"5.6.7.8"}`); rec.Code != http.StatusOK {
		t.Fatalf("save status = %d", rec.Code)
	}
	env.api.publicIPMu.Lock()
	cached := env.api.publicIPCached
	env.api.publicIPMu.Unlock()
	if cached != "" {
		t.Fatalf("probe cache was not invalidated: %q", cached)
	}
}
