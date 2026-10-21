package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"ts3panel/internal/orchestrator"
	"ts3panel/internal/store"
)

func testConfigWithProviders() store.Config {
	return store.Config{
		Providers: []store.ProviderConfig{
			{ID: "cf-main", Type: "cloudflare", Zone: "example.com", Enabled: true, Default: true},
			{ID: "manual-main", Type: "manual", Zone: "other.test", Enabled: true},
		},
	}
}

func TestResolveInstanceAddress(t *testing.T) {
	cfg := testConfigWithProviders()
	cases := []struct {
		name       string
		input      string
		providerID string
		wantID     string
		wantFQDN   string
		wantErr    bool
	}{
		{name: "prefix without provider", input: "ts1", wantID: "ts1", wantFQDN: "ts1"},
		{name: "prefix with provider zone", input: "ts1", providerID: "cf-main", wantID: "ts1", wantFQDN: "ts1.example.com"},
		{name: "fqdn without provider", input: "ts1.example.com", wantID: "ts1", wantFQDN: "ts1.example.com"},
		{name: "fqdn within provider zone", input: "ts1.example.com", providerID: "cf-main", wantID: "ts1", wantFQDN: "ts1.example.com"},
		{name: "uppercase normalized", input: "TS1", wantID: "ts1", wantFQDN: "ts1"},
		{name: "trailing dot", input: "ts1.example.com.", wantID: "ts1", wantFQDN: "ts1.example.com"},
		{name: "empty", input: "", wantErr: true},
		{name: "underscore", input: "bad_name", wantErr: true},
		{name: "too long", input: "abcdefghijklmnopqrstu", wantErr: true},
		{name: "unknown provider", input: "ts1", providerID: "nope", wantErr: true},
		{name: "fqdn outside provider zone", input: "ts1.elsewhere.test", providerID: "cf-main", wantErr: true},
		{name: "invalid fqdn", input: "ts1..example.com", wantErr: true},
		{name: "dot input without valid label", input: "-bad.example.com", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			id, fqdn, _, err := resolveInstanceAddress(cfg, tc.input, tc.providerID)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got id=%q fqdn=%q", id, fqdn)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if id != tc.wantID || fqdn != tc.wantFQDN {
				t.Fatalf("id=%q fqdn=%q, want %q %q", id, fqdn, tc.wantID, tc.wantFQDN)
			}
		})
	}
}

func TestResolveInstanceAddressUnknownProviderIsTyped(t *testing.T) {
	if _, _, _, err := resolveInstanceAddress(testConfigWithProviders(), "ts1", "nope"); !errors.Is(err, errProviderNotFound) {
		t.Fatalf("err = %v, want errProviderNotFound", err)
	}
}

func TestParseLimits(t *testing.T) {
	if got := parseMemoryLimit("512m"); got != 512*1024*1024 {
		t.Fatalf("512m = %d", got)
	}
	if got := parseMemoryLimit("1g"); got != 1024*1024*1024 {
		t.Fatalf("1g = %d", got)
	}
	if got := parseMemoryLimit("1024"); got != 1024 {
		t.Fatalf("1024 = %d", got)
	}
	for _, bad := range []string{"", "abc", "0", "-5", "10x"} {
		if got := parseMemoryLimit(bad); got != 0 {
			t.Fatalf("parseMemoryLimit(%q) = %d, want 0", bad, got)
		}
	}
	if got := parseCPULimit("1.0"); got != 1_000_000_000 {
		t.Fatalf("1.0 = %d", got)
	}
	if got := parseCPULimit("0.5"); got != 500_000_000 {
		t.Fatalf("0.5 = %d", got)
	}
	for _, bad := range []string{"", "abc", "0", "-1"} {
		if got := parseCPULimit(bad); got != 0 {
			t.Fatalf("parseCPULimit(%q) = %d, want 0", bad, got)
		}
	}
}

func TestWriteInstanceErrorCodes(t *testing.T) {
	cases := []struct {
		err        error
		wantStatus int
		wantCode   string
	}{
		{orchestrator.ErrInstanceNotFound, http.StatusNotFound, "E_INSTANCE_NOT_FOUND"},
		{orchestrator.ErrSubdomainTaken, http.StatusConflict, "E_SUBDOMAIN_TAKEN"},
		{orchestrator.ErrSubdomainInvalid, http.StatusBadRequest, "E_SUBDOMAIN_INVALID"},
		{orchestrator.ErrLimitReached, http.StatusConflict, "E_LIMIT_REACHED"},
		{orchestrator.ErrPortExhausted, http.StatusConflict, "E_PORT_EXHAUSTED"},
		{orchestrator.ErrContainerTimeout, http.StatusInternalServerError, "E_CONTAINER_TIMEOUT"},
		{orchestrator.ErrNotManaged, http.StatusConflict, "E_DOCKER"},
		{orchestrator.ErrDocker, http.StatusInternalServerError, "E_DOCKER"},
	}
	for _, tc := range cases {
		rec := httptest.NewRecorder()
		writeInstanceError(rec, tc.err)
		if rec.Code != tc.wantStatus {
			t.Fatalf("%v: status = %d, want %d", tc.err, rec.Code, tc.wantStatus)
		}
		var body struct {
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("%v: decode: %v", tc.err, err)
		}
		if body.Error.Code != tc.wantCode {
			t.Fatalf("%v: code = %q, want %q", tc.err, body.Error.Code, tc.wantCode)
		}
	}
}
