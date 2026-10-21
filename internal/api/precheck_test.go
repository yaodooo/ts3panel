package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"ts3panel/internal/model"
)

func precheck(t *testing.T, a *API, params url.Values) (int, domainPrecheckResponse) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/domain/precheck?"+params.Encode(), nil)
	rec := httptest.NewRecorder()
	a.handleDomainPrecheck(rec, req)
	var resp domainPrecheckResponse
	if rec.Body.Len() > 0 {
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode precheck: %v (body=%s)", err, rec.Body.String())
		}
	}
	return rec.Code, resp
}

func TestPrecheckAvailable(t *testing.T) {
	env := newDNSTestEnv(t, dnsTestOptions{withFactory: true})
	code, resp := precheck(t, env.api, url.Values{"subdomain": {"ts2"}, "provider_id": {"cf-main"}})
	if code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	if !resp.Available || !resp.Checked || len(resp.Conflicts) != 0 {
		t.Fatalf("resp = %+v", resp)
	}
	if resp.FQDN != "ts2.example.com" || resp.Zone != "example.com" {
		t.Fatalf("fqdn=%q zone=%q", resp.FQDN, resp.Zone)
	}
}

func TestPrecheckDetectsExistingRecords(t *testing.T) {
	env := newDNSTestEnv(t, dnsTestOptions{withFactory: true})
	env.fake.seed("A", "ts2.example.com", "existing-a")
	env.fake.seed("SRV", "ts2.example.com", "existing-srv")

	code, resp := precheck(t, env.api, url.Values{"subdomain": {"ts2"}, "provider_id": {"cf-main"}})
	if code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	if resp.Available || !resp.Checked || resp.LocalConflict {
		t.Fatalf("resp = %+v", resp)
	}
	if len(resp.Conflicts) != 2 {
		t.Fatalf("conflicts = %v", resp.Conflicts)
	}
}

func TestPrecheckLocalDuplicate(t *testing.T) {
	env := newDNSTestEnv(t, dnsTestOptions{withFactory: true})
	// The seeded instance is ts1, so re-using the prefix (even without a zone)
	// must be rejected before a container is created.
	code, resp := precheck(t, env.api, url.Values{"subdomain": {"ts1"}})
	if code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	if resp.Available || !resp.LocalConflict {
		t.Fatalf("resp = %+v", resp)
	}
}

func TestPrecheckManualProviderSkipsLookup(t *testing.T) {
	env := newDNSTestEnv(t, dnsTestOptions{providerType: "manual"})
	code, resp := precheck(t, env.api, url.Values{"subdomain": {"ts2"}, "provider_id": {"cf-main"}})
	if code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	if !resp.Available || resp.Checked {
		t.Fatalf("manual precheck must not claim it checked records: %+v", resp)
	}
	if resp.Message == "" {
		t.Fatal("manual precheck has no explanation")
	}
}

func TestPrecheckWithoutProvider(t *testing.T) {
	env := newDNSTestEnv(t, dnsTestOptions{withFactory: true})
	code, resp := precheck(t, env.api, url.Values{"subdomain": {"ts9"}})
	if code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	if !resp.Available || resp.Checked {
		t.Fatalf("resp = %+v", resp)
	}
	if resp.FQDN != "ts9" {
		t.Fatalf("fqdn = %q", resp.FQDN)
	}
}

func TestPrecheckExcludesTheInstanceBeingRebound(t *testing.T) {
	env := newDNSTestEnv(t, dnsTestOptions{withFactory: true})
	code, resp := precheck(t, env.api, url.Values{
		"subdomain":   {"ts1"},
		"provider_id": {"cf-main"},
		"instance_id": {"ts1"},
	})
	if code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	if resp.LocalConflict || !resp.Available {
		t.Fatalf("rebinding an instance must not conflict with itself: %+v", resp)
	}
}

func TestPrecheckTreatsOwnedRecordsAsNoConflict(t *testing.T) {
	instance := model.Instance{
		ID: "ts1", Name: "一队", Subdomain: "ts1.example.com", ProviderID: "cf-main",
		Ports:     model.Ports{Voice: 9987, FileTransfer: 30033, Query: 10011, QuerySSH: 10022},
		Container: "ts3-ts1", Status: model.InstanceStatusReady, DNSStatus: model.DNSStatusVerified,
		DNS: model.DNSInfo{ARecordID: "own-a", SRVRecordID: "own-srv"},
	}
	env := newDNSTestEnv(t, dnsTestOptions{withFactory: true, instance: instance})
	env.fake.seed("A", "ts1.example.com", "own-a")
	env.fake.seed("SRV", "ts1.example.com", "own-srv")

	code, resp := precheck(t, env.api, url.Values{
		"subdomain":   {"ts1"},
		"provider_id": {"cf-main"},
		"instance_id": {"ts1"},
	})
	if code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	if !resp.Available || len(resp.Conflicts) != 0 {
		t.Fatalf("panel-owned records must not block a rebind: %+v", resp)
	}
}

func TestPrecheckValidationErrors(t *testing.T) {
	env := newDNSTestEnv(t, dnsTestOptions{withFactory: true})

	code, _ := precheck(t, env.api, url.Values{"subdomain": {"bad_name"}})
	if code != http.StatusBadRequest {
		t.Fatalf("invalid subdomain status = %d", code)
	}
	code, _ = precheck(t, env.api, url.Values{"subdomain": {"ts2"}, "provider_id": {"missing"}})
	if code != http.StatusBadRequest {
		t.Fatalf("unknown provider status = %d", code)
	}
	code, _ = precheck(t, env.api, url.Values{"subdomain": {"ts2.other.test"}, "provider_id": {"cf-main"}})
	if code != http.StatusBadRequest {
		t.Fatalf("zone mismatch status = %d", code)
	}
}
