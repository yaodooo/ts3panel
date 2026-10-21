package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"ts3panel/internal/store"
)

func providerCall(t *testing.T, handler func(http.ResponseWriter, *http.Request), method, target, body, id string) *httptest.ResponseRecorder {
	t.Helper()
	var reader *strings.Reader
	if body == "" {
		reader = strings.NewReader("")
	} else {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, target, reader)
	req.Header.Set("Content-Type", "application/json")
	if id != "" {
		req.SetPathValue("id", id)
	}
	rec := httptest.NewRecorder()
	handler(rec, req)
	return rec
}

func TestProviderCreateMasksAndEncryptsCredentials(t *testing.T) {
	env := newDNSTestEnv(t, dnsTestOptions{withFactory: true})
	rec := providerCall(t, env.api.handleCreateProvider, http.MethodPost, "/api/providers",
		`{"type":"cloudflare","label":"CF 备用","zone":"other.test","creds":{"api_token":"abcdefghijkl"}}`, "")
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var view providerView
	if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if view.ID == "" || view.Zone != "other.test" || !view.Configured {
		t.Fatalf("view = %+v", view)
	}
	if view.Creds["api_token"] != "abcd****ijkl" {
		t.Fatalf("masked token = %q", view.Creds["api_token"])
	}

	cfg := env.config.Snapshot()
	var stored *store.ProviderConfig
	for i := range cfg.Providers {
		if cfg.Providers[i].Zone == "other.test" {
			stored = &cfg.Providers[i]
		}
	}
	if stored == nil {
		t.Fatal("provider not persisted")
	}
	if stored.CredsEnc == "" {
		t.Fatal("credentials were not encrypted")
	}
	if strings.Contains(stored.CredsEnc, "abcdefghijkl") {
		t.Fatal("plaintext token leaked into config.toml")
	}

	list := providerCall(t, env.api.handleListProviders, http.MethodGet, "/api/providers", "", "")
	if list.Code != http.StatusOK {
		t.Fatalf("list status = %d", list.Code)
	}
	if strings.Contains(list.Body.String(), "abcdefghijkl") {
		t.Fatal("plaintext token leaked through the list endpoint")
	}
}

func TestProviderTestEndpointReturnsZones(t *testing.T) {
	env := newDNSTestEnv(t, dnsTestOptions{withFactory: true})
	rec := providerCall(t, env.api.handleTestProvider, http.MethodPost, "/api/providers/test",
		`{"id":"cf-main"}`, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var resp providerTestResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp.OK || !resp.ZoneFound || len(resp.Zones) != 1 || resp.Zones[0].Name != "example.com" {
		t.Fatalf("resp = %+v", resp)
	}
	// A successful test caches the vendor zone id in config.toml.
	var storedZoneID string
	for _, p := range env.config.Snapshot().Providers {
		if p.ID == "cf-main" {
			storedZoneID = p.ZoneID
		}
	}
	if storedZoneID != "z1" {
		t.Fatalf("zone_id = %q, want z1", storedZoneID)
	}

	rec = providerCall(t, env.api.handleTestProvider, http.MethodPost, "/api/providers/test",
		`{"id":"cf-main","zone":"nope.test"}`, "")
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp.OK || resp.ZoneFound {
		t.Fatalf("resp = %+v, want ok with zone_found=false", resp)
	}
}

func TestProviderUpdateKeepsMaskedCredential(t *testing.T) {
	env := newDNSTestEnv(t, dnsTestOptions{withFactory: true})
	created := providerCall(t, env.api.handleCreateProvider, http.MethodPost, "/api/providers",
		`{"type":"dnspod","zone":"other.test","creds":{"secret_id":"AKIDsecretid","secret_key":"secretkeyvalue"}}`, "")
	if created.Code != http.StatusCreated {
		t.Fatalf("create status = %d, body=%s", created.Code, created.Body.String())
	}
	var view providerView
	if err := json.Unmarshal(created.Body.Bytes(), &view); err != nil {
		t.Fatalf("decode: %v", err)
	}

	// Renaming must not require re-entering the mask.
	updated := providerCall(t, env.api.handleUpdateProvider, http.MethodPut, "/api/providers/"+view.ID,
		`{"label":"改名后的 DNSPod"}`, view.ID)
	if updated.Code != http.StatusOK {
		t.Fatalf("update status = %d, body=%s", updated.Code, updated.Body.String())
	}
	var after providerView
	if err := json.Unmarshal(updated.Body.Bytes(), &after); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if after.Label != "改名后的 DNSPod" || !after.Configured {
		t.Fatalf("after = %+v", after)
	}
	if after.Creds["secret_key"] != "secr****alue" {
		t.Fatalf("masked secret = %q", after.Creds["secret_key"])
	}

	// A settings form may post the mask back; that must not overwrite the
	// stored secret.
	echoed := providerCall(t, env.api.handleUpdateProvider, http.MethodPut, "/api/providers/"+view.ID,
		`{"label":"再次改名","creds":{"secret_id":"AKID****etid","secret_key":"secr****alue"}}`, view.ID)
	if echoed.Code != http.StatusOK {
		t.Fatalf("mask echo update status = %d, body=%s", echoed.Code, echoed.Body.String())
	}
	var echoedView providerView
	if err := json.Unmarshal(echoed.Body.Bytes(), &echoedView); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !echoedView.Configured || echoedView.Creds["secret_key"] != "secr****alue" {
		t.Fatalf("mask echo damaged credentials: %+v", echoedView)
	}
	if echoedView.Label != "再次改名" {
		t.Fatalf("label = %q", echoedView.Label)
	}
}

func TestProviderDeleteInUseIsRefused(t *testing.T) {
	env := newDNSTestEnv(t, dnsTestOptions{withFactory: true})
	if err := env.state.Update(func(st *store.State) error {
		for i := range st.Instances {
			st.Instances[i].ProviderID = "cf-main"
		}
		return nil
	}); err != nil {
		t.Fatalf("mark provider in use: %v", err)
	}

	rec := providerCall(t, env.api.handleDeleteProvider, http.MethodDelete, "/api/providers/cf-main", "", "cf-main")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("delete in-use status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if err := env.state.Update(func(st *store.State) error {
		for i := range st.Instances {
			st.Instances[i].ProviderID = ""
		}
		return nil
	}); err != nil {
		t.Fatalf("clear provider: %v", err)
	}
	rec = providerCall(t, env.api.handleDeleteProvider, http.MethodDelete, "/api/providers/cf-main", "", "cf-main")
	if rec.Code != http.StatusOK {
		t.Fatalf("delete status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if len(env.config.Snapshot().Providers) != 0 {
		t.Fatal("provider was not removed")
	}
}

func TestProviderRoutesRequireAuth(t *testing.T) {
	env := newDNSTestEnv(t, dnsTestOptions{withFactory: true})
	handler := env.api.Handler()
	req := httptest.NewRequest(http.MethodGet, "/api/providers", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "E_AUTH") {
		t.Fatalf("body = %s", rec.Body.String())
	}
}
