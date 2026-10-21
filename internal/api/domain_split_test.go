package api

import (
	"encoding/json"
	"net/http"
	"testing"

	"ts3panel/internal/model"
)

func TestBindDomainCanDeferVerification(t *testing.T) {
	env := newDNSTestEnv(t, dnsTestOptions{withFactory: true})

	rec := bindRequest(t, env.api, "ts1", `{"subdomain":"ts1","provider_id":"cf-main","verify":false}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var resp domainBindResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.DNSStatus != model.DNSStatusPending || resp.Verified {
		t.Fatalf("resp = %+v", resp)
	}
	if resp.ARecordID == "" || resp.SRVRecordID == "" || !resp.Retryable {
		t.Fatalf("record ids lost: %+v", resp)
	}
	inst := instanceState(t, env.state, "ts1")
	if inst.DNSStatus != model.DNSStatusPending {
		t.Fatalf("state dns_status = %q, want pending", inst.DNSStatus)
	}

	// The wizard's step 4 calls the existing verify endpoint.
	rec = verifyRequest(t, env.api, "ts1")
	if rec.Code != http.StatusOK {
		t.Fatalf("verify status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var verify domainVerifyResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &verify); err != nil {
		t.Fatalf("decode verify: %v", err)
	}
	if !verify.Verified || verify.DNSStatus != model.DNSStatusVerified {
		t.Fatalf("verify resp = %+v", verify)
	}
	if inst := instanceState(t, env.state, "ts1"); inst.DNSStatus != model.DNSStatusVerified {
		t.Fatalf("state after verify = %q", inst.DNSStatus)
	}
}
