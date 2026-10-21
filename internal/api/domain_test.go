package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"ts3panel/internal/dns"
	"ts3panel/internal/model"
	"ts3panel/internal/store"

	"golang.org/x/crypto/bcrypt"
)

// testAdminPassword is seeded with bcrypt.MinCost in DNS API tests; production
// hashes always use cost 12.
const testAdminPassword = "AdminPassw0rd1"

// fakeProvider is an in-memory DNS provider used to exercise the API without
// any vendor traffic. It implements both the required interface and the
// optional RecordFinder used by the conflict pre-check.
type fakeProvider struct {
	mu          sync.Mutex
	zone        string
	records     map[string]string // "A|fqdn" -> id
	byID        map[string]string // id -> key
	next        int
	failA       error
	failSRV     error
	deleted     []string
	lastAIP     string
	lastSRVPort int
	lastSRVFQDN string
}

func newFakeProvider(zone string) *fakeProvider {
	return &fakeProvider{zone: zone, records: map[string]string{}, byID: map[string]string{}}
}

func (f *fakeProvider) Validate(context.Context) ([]dns.Zone, error) {
	return []dns.Zone{{ID: "z1", Name: f.zone}}, nil
}

func (f *fakeProvider) LookupA(_ context.Context, _, fqdn string) (dns.RecordID, error) {
	return f.lookup("A|" + fqdn)
}

func (f *fakeProvider) LookupSRV(_ context.Context, _, fqdn string) (dns.RecordID, error) {
	return f.lookup("SRV|" + fqdn)
}

func (f *fakeProvider) lookup(key string) (dns.RecordID, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if id, ok := f.records[key]; ok {
		return dns.RecordID(id), nil
	}
	return "", dns.ErrRecordNotFound
}

func (f *fakeProvider) seed(recordType, fqdn, id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := recordType + "|" + fqdn
	f.records[key] = id
	f.byID[id] = key
}

func (f *fakeProvider) UpsertA(_ context.Context, _, fqdn, ip string, ttl int) (dns.RecordID, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failA != nil {
		return "", f.failA
	}
	if ip == "" || ttl <= 0 {
		return "", fmt.Errorf("fake provider got invalid A parameters ip=%q ttl=%d", ip, ttl)
	}
	f.lastAIP = ip
	return f.upsertLocked("A|" + fqdn)
}

func (f *fakeProvider) UpsertSRV(_ context.Context, _, fqdn string, port, ttl int) (dns.RecordID, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failSRV != nil {
		return "", f.failSRV
	}
	if port <= 0 || ttl <= 0 {
		return "", fmt.Errorf("fake provider got invalid SRV parameters port=%d ttl=%d", port, ttl)
	}
	f.lastSRVPort = port
	f.lastSRVFQDN = fqdn
	return f.upsertLocked("SRV|" + fqdn)
}

func (f *fakeProvider) upsertLocked(key string) (dns.RecordID, error) {
	if id, ok := f.records[key]; ok {
		return dns.RecordID(id), nil
	}
	f.next++
	id := fmt.Sprintf("rec-%d", f.next)
	f.records[key] = id
	f.byID[id] = key
	return dns.RecordID(id), nil
}

func (f *fakeProvider) DeleteRecord(_ context.Context, _ string, id dns.RecordID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	key, ok := f.byID[string(id)]
	if !ok {
		return nil
	}
	delete(f.byID, string(id))
	delete(f.records, key)
	f.deleted = append(f.deleted, string(id))
	return nil
}

func (f *fakeProvider) count(prefix string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for key := range f.records {
		if strings.HasPrefix(key, prefix+"|") {
			n++
		}
	}
	return n
}

func (f *fakeProvider) deletes() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.deleted...)
}

func (f *fakeProvider) lastUpsert() (srvPort int, aIP, srvFQDN string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastSRVPort, f.lastAIP, f.lastSRVFQDN
}

func okVerifier() *dns.Verifier {
	return &dns.Verifier{
		LookupHost: func(context.Context, string) ([]string, error) {
			return []string{"1.2.3.4"}, nil
		},
		LookupSRV: func(_ context.Context, _, _, name string) (string, []*net.SRV, error) {
			return "", []*net.SRV{{Target: name + ".", Port: 9987}}, nil
		},
		Now: time.Now, Interval: time.Millisecond, Timeout: 50 * time.Millisecond,
	}
}

type dnsTestEnv struct {
	api    *API
	fake   *fakeProvider
	config *store.ConfigStore
	state  *store.StateStore
}

type dnsTestOptions struct {
	providerType string
	withFactory  bool
	verifier     *dns.Verifier
	publicIP     string
	instance     model.Instance
}

func newDNSTestEnv(t *testing.T, opts dnsTestOptions) *dnsTestEnv {
	t.Helper()
	dir := t.TempDir()
	configStore, _, err := store.LoadConfig(filepath.Join(dir, "config.toml"))
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	cfg := configStore.Snapshot()
	typ := opts.providerType
	if typ == "" {
		typ = "cloudflare"
	}
	cfg.Providers = []store.ProviderConfig{{
		ID: "cf-main", Type: typ, Label: "test", Zone: "example.com", Enabled: true, Default: true,
	}}
	if typ != "manual" {
		cipher, cipherErr := store.NewCredentialCipher(cfg.MasterKey)
		if cipherErr != nil {
			t.Fatalf("credential cipher: %v", cipherErr)
		}
		creds := map[string]string{}
		switch typ {
		case "cloudflare":
			creds["api_token"] = "test-token-value"
		case "aliyun":
			creds["access_key_id"] = "test-key-id"
			creds["access_key_secret"] = "test-key-secret"
		case "dnspod":
			creds["secret_id"] = "test-secret-id"
			creds["secret_key"] = "test-secret-key"
		}
		plain, marshalErr := json.Marshal(creds)
		if marshalErr != nil {
			t.Fatalf("marshal creds: %v", marshalErr)
		}
		encoded, encryptErr := cipher.Encrypt(plain)
		if encryptErr != nil {
			t.Fatalf("encrypt creds: %v", encryptErr)
		}
		cfg.Providers[0].CredsEnc = encoded
	}
	if err := configStore.Save(cfg); err != nil {
		t.Fatalf("save config: %v", err)
	}

	stateStore, err := store.OpenState(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatalf("open state: %v", err)
	}
	instance := opts.instance
	if instance.ID == "" {
		instance = model.Instance{
			ID:        "ts1",
			Name:      "一队",
			Subdomain: "ts1",
			Ports:     model.Ports{Voice: 9987, FileTransfer: 30033, Query: 10011, QuerySSH: 10022},
			Container: "ts3-ts1",
			Status:    model.InstanceStatusReady,
			DNSStatus: model.DNSStatusNone,
			CreatedAt: time.Now().UTC(),
		}
	}
	if err := stateStore.Update(func(st *store.State) error {
		st.SetupCompleted = true
		pwHash, hashErr := bcrypt.GenerateFromPassword([]byte(testAdminPassword), bcrypt.MinCost)
		if hashErr != nil {
			return hashErr
		}
		st.User = &model.User{Username: "admin", PasswordHash: string(pwHash), CreatedAt: time.Now().UTC()}
		st.Instances = []model.Instance{instance}
		return nil
	}); err != nil {
		t.Fatalf("seed state: %v", err)
	}

	fake := newFakeProvider("example.com")
	verifier := opts.verifier
	if verifier == nil {
		verifier = okVerifier()
	}
	publicIP := opts.publicIP
	if publicIP == "" {
		publicIP = "1.2.3.4"
	}
	var factory func(store.ProviderConfig, map[string]string) (dns.Provider, error)
	if opts.withFactory {
		factory = func(store.ProviderConfig, map[string]string) (dns.Provider, error) {
			return fake, nil
		}
	}
	apiHandler, err := New(Options{
		Config:           configStore,
		State:            stateStore,
		Version:          "test",
		ProviderFactory:  factory,
		Verifier:         verifier,
		PublicIPDetector: func(context.Context) (string, error) { return publicIP, nil },
	})
	if err != nil {
		t.Fatalf("new api: %v", err)
	}
	return &dnsTestEnv{api: apiHandler, fake: fake, config: configStore, state: stateStore}
}

func bindRequest(t *testing.T, a *API, id, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/instances/"+id+"/domain", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("id", id)
	rec := httptest.NewRecorder()
	a.handleBindDomain(rec, req)
	return rec
}

func verifyRequest(t *testing.T, a *API, id string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/instances/"+id+"/domain/verify", nil)
	req.SetPathValue("id", id)
	rec := httptest.NewRecorder()
	a.handleVerifyDomain(rec, req)
	return rec
}

func unbindRequest(t *testing.T, a *API, id string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodDelete, "/api/instances/"+id+"/domain", nil)
	req.SetPathValue("id", id)
	rec := httptest.NewRecorder()
	a.handleUnbindDomain(rec, req)
	return rec
}

func decodeBind(t *testing.T, rec *httptest.ResponseRecorder) domainBindResponse {
	t.Helper()
	var resp domainBindResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode bind response: %v (body=%s)", err, rec.Body.String())
	}
	return resp
}

func instanceState(t *testing.T, state *store.StateStore, id string) model.Instance {
	t.Helper()
	st, err := state.Snapshot()
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	for _, inst := range st.Instances {
		if inst.ID == id {
			return inst
		}
	}
	t.Fatalf("instance %q not found", id)
	return model.Instance{}
}

func TestBindDomainWritesAndVerifies(t *testing.T) {
	env := newDNSTestEnv(t, dnsTestOptions{withFactory: true})
	rec := bindRequest(t, env.api, "ts1", `{"subdomain":"ts1","provider_id":"cf-main"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	resp := decodeBind(t, rec)
	if !resp.OK || !resp.Verified || resp.DNSStatus != model.DNSStatusVerified {
		t.Fatalf("response = %+v", resp)
	}
	if resp.Subdomain != "ts1.example.com" || resp.ARecordID == "" || resp.SRVRecordID == "" {
		t.Fatalf("response = %+v", resp)
	}

	inst := instanceState(t, env.state, "ts1")
	if inst.Subdomain != "ts1.example.com" || inst.ProviderID != "cf-main" {
		t.Fatalf("instance = %+v", inst)
	}
	if inst.DNSStatus != model.DNSStatusVerified || inst.DNS.VerifiedAt == nil {
		t.Fatalf("dns state = %+v", inst.DNS)
	}
	if inst.DNS.ARecordID != resp.ARecordID || inst.DNS.SRVRecordID != resp.SRVRecordID {
		t.Fatalf("record ids not persisted: %+v", inst.DNS)
	}
	if env.fake.count("A") != 1 || env.fake.count("SRV") != 1 {
		t.Fatalf("fake records A=%d SRV=%d", env.fake.count("A"), env.fake.count("SRV"))
	}
}

func TestBindDomainFailureKeepsInstance(t *testing.T) {
	env := newDNSTestEnv(t, dnsTestOptions{withFactory: true})
	env.fake.failSRV = dns.Errorf(dns.CodePerm, "synthetic permission failure")

	rec := bindRequest(t, env.api, "ts1", `{"subdomain":"ts1","provider_id":"cf-main"}`)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var failure domainFailureResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &failure); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if failure.Error.Code != dns.CodePerm {
		t.Fatalf("code = %q", failure.Error.Code)
	}
	if !failure.Retryable || len(failure.Manual) != 2 {
		t.Fatalf("failure = %+v", failure)
	}
	if len(failure.Missing) != 1 || failure.Missing[0] != "SRV" {
		t.Fatalf("missing = %v", failure.Missing)
	}

	inst := instanceState(t, env.state, "ts1")
	if inst.Status != model.InstanceStatusReady {
		t.Fatalf("instance status changed to %q; DNS failure must not touch it", inst.Status)
	}
	if inst.DNSStatus != model.DNSStatusFailed {
		t.Fatalf("dns_status = %q", inst.DNSStatus)
	}
	if inst.DNS.ARecordID == "" {
		t.Fatal("A record ID was not persisted after the partial write")
	}
	if env.fake.count("A") != 1 || env.fake.count("SRV") != 0 {
		t.Fatalf("fake records A=%d SRV=%d", env.fake.count("A"), env.fake.count("SRV"))
	}
}

func TestBindDomainAuthFailureKeepsInstance(t *testing.T) {
	env := newDNSTestEnv(t, dnsTestOptions{withFactory: true})
	env.fake.failA = dns.Errorf(dns.CodeAuth, "synthetic invalid token")

	rec := bindRequest(t, env.api, "ts1", `{"subdomain":"ts1","provider_id":"cf-main"}`)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var failure domainFailureResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &failure); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if failure.Error.Code != dns.CodeAuth {
		t.Fatalf("code = %q, want %q", failure.Error.Code, dns.CodeAuth)
	}
	if !failure.Retryable || len(failure.Manual) != 2 {
		t.Fatalf("failure = %+v", failure)
	}
	inst := instanceState(t, env.state, "ts1")
	if inst.Status != model.InstanceStatusReady || inst.DNSStatus != model.DNSStatusFailed {
		t.Fatalf("instance = %+v", inst)
	}
	if env.fake.count("A") != 0 || env.fake.count("SRV") != 0 {
		t.Fatalf("credentials failure still wrote records: A=%d SRV=%d", env.fake.count("A"), env.fake.count("SRV"))
	}
}

func TestBindDomainTwiceIsIdempotent(t *testing.T) {
	env := newDNSTestEnv(t, dnsTestOptions{withFactory: true})
	first := decodeBind(t, bindRequest(t, env.api, "ts1", `{"subdomain":"ts1","provider_id":"cf-main"}`))
	second := decodeBind(t, bindRequest(t, env.api, "ts1", `{"subdomain":"ts1","provider_id":"cf-main"}`))
	if first.ARecordID != second.ARecordID || first.SRVRecordID != second.SRVRecordID {
		t.Fatalf("ids changed: %+v -> %+v", first, second)
	}
	if env.fake.count("A") != 1 || env.fake.count("SRV") != 1 {
		t.Fatalf("records A=%d SRV=%d, want 1 each", env.fake.count("A"), env.fake.count("SRV"))
	}
	if second.DNSStatus != model.DNSStatusVerified {
		t.Fatalf("second bind dns_status = %q", second.DNSStatus)
	}
}

func TestBindDomainConflictNeedsOverwrite(t *testing.T) {
	env := newDNSTestEnv(t, dnsTestOptions{withFactory: true})
	env.fake.seed("A", "ts1.example.com", "foreign-a")

	rec := bindRequest(t, env.api, "ts1", `{"subdomain":"ts1","provider_id":"cf-main"}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var failure domainFailureResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &failure); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if failure.Error.Code != dns.CodeConflict {
		t.Fatalf("code = %q", failure.Error.Code)
	}
	if len(failure.Manual) != 2 {
		t.Fatalf("manual = %+v", failure.Manual)
	}
	inst := instanceState(t, env.state, "ts1")
	if inst.DNSStatus != model.DNSStatusNone || inst.Subdomain != "ts1" {
		t.Fatalf("conflict changed instance: %+v", inst)
	}
	if id, _ := env.fake.LookupA(context.Background(), "example.com", "ts1.example.com"); string(id) != "foreign-a" {
		t.Fatalf("foreign A record was overwritten without overwrite=true: %q", id)
	}

	ok := decodeBind(t, bindRequest(t, env.api, "ts1", `{"subdomain":"ts1","provider_id":"cf-main","overwrite":true}`))
	if !ok.Verified {
		t.Fatalf("overwrite bind not verified: %+v", ok)
	}
	if ok.ARecordID != "foreign-a" {
		t.Fatalf("ARecordID = %q, want the taken-over foreign id", ok.ARecordID)
	}
	if env.fake.count("A") != 1 {
		t.Fatalf("A records = %d after overwrite", env.fake.count("A"))
	}
}

func TestBindDomainUsesInstanceVoicePort(t *testing.T) {
	verifier := okVerifier()
	verifier.LookupSRV = func(_ context.Context, _, _, name string) (string, []*net.SRV, error) {
		return "", []*net.SRV{{Target: name + ".", Port: 9988}}, nil
	}
	inst := model.Instance{
		ID:        "ts2",
		Name:      "二队",
		Subdomain: "ts2",
		Ports:     model.Ports{Voice: 9988, FileTransfer: 30034, Query: 10012, QuerySSH: 10023},
		Container: "ts3-ts2",
		Status:    model.InstanceStatusReady,
		DNSStatus: model.DNSStatusNone,
		CreatedAt: time.Now().UTC(),
	}
	env := newDNSTestEnv(t, dnsTestOptions{withFactory: true, verifier: verifier, instance: inst})
	resp := decodeBind(t, bindRequest(t, env.api, "ts2", `{"subdomain":"ts2","provider_id":"cf-main"}`))
	if !resp.Verified {
		t.Fatalf("bind not verified: %+v", resp)
	}
	port, ip, fqdn := env.fake.lastUpsert()
	if port != 9988 {
		t.Fatalf("SRV port = %d, want the instance voice port 9988", port)
	}
	if ip != "1.2.3.4" || fqdn != "ts2.example.com" {
		t.Fatalf("A ip=%q fqdn=%q", ip, fqdn)
	}
}

func TestManualBindReturnsChecklistAndVerifyWorks(t *testing.T) {
	env := newDNSTestEnv(t, dnsTestOptions{providerType: "manual"})
	rec := bindRequest(t, env.api, "ts1", `{"subdomain":"ts1","provider_id":"cf-main"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	resp := decodeBind(t, rec)
	if resp.DNSStatus != model.DNSStatusNone || len(resp.Manual) != 2 {
		t.Fatalf("response = %+v", resp)
	}
	if resp.Manual[1].Value != "0 5 9987 ts1.example.com" || resp.Manual[1].Host != "_ts3._udp.ts1" {
		t.Fatalf("SRV checklist = %+v", resp.Manual[1])
	}
	if !strings.Contains(resp.ManualText, "_ts3._udp.ts1") || !strings.Contains(resp.ManualText, "0 5 9987 ts1.example.com") {
		t.Fatalf("manual text = %q", resp.ManualText)
	}
	inst := instanceState(t, env.state, "ts1")
	if inst.Subdomain != "ts1.example.com" || inst.DNSStatus != model.DNSStatusNone {
		t.Fatalf("instance = %+v", inst)
	}

	rec = verifyRequest(t, env.api, "ts1")
	if rec.Code != http.StatusOK {
		t.Fatalf("verify status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var verify domainVerifyResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &verify); err != nil {
		t.Fatalf("decode verify: %v", err)
	}
	if !verify.Verified || verify.DNSStatus != model.DNSStatusVerified {
		t.Fatalf("verify = %+v", verify)
	}
}

func TestUnbindDomainDeletesByIDInOrder(t *testing.T) {
	env := newDNSTestEnv(t, dnsTestOptions{withFactory: true})
	bind := decodeBind(t, bindRequest(t, env.api, "ts1", `{"subdomain":"ts1","provider_id":"cf-main"}`))
	if !bind.Verified {
		t.Fatalf("bind failed: %+v", bind)
	}

	rec := unbindRequest(t, env.api, "ts1")
	if rec.Code != http.StatusOK {
		t.Fatalf("unbind status = %d, body=%s", rec.Code, rec.Body.String())
	}
	deleted := env.fake.deletes()
	if len(deleted) != 2 || deleted[0] != bind.SRVRecordID || deleted[1] != bind.ARecordID {
		t.Fatalf("delete order = %v, want SRV then A (%s, %s)", deleted, bind.SRVRecordID, bind.ARecordID)
	}
	inst := instanceState(t, env.state, "ts1")
	if inst.DNSStatus != model.DNSStatusNone || inst.DNS.ARecordID != "" || inst.DNS.SRVRecordID != "" {
		t.Fatalf("instance after unbind = %+v", inst)
	}
	// The instance itself is retained (F-13).
	st, err := env.state.Snapshot()
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if len(st.Instances) != 1 {
		t.Fatal("instance was removed by unbind")
	}
	// Unbind is idempotent.
	if rec := unbindRequest(t, env.api, "ts1"); rec.Code != http.StatusOK {
		t.Fatalf("second unbind status = %d", rec.Code)
	}
}

// TestDomainEndpointsOverHTTP exercises the real mux: login session cookie,
// CSRF header, route registration and the manual checklist endpoint.
func TestDomainEndpointsOverHTTP(t *testing.T) {
	env := newDNSTestEnv(t, dnsTestOptions{providerType: "manual"})
	handler := env.api.Handler()

	login := httptest.NewRequest(http.MethodPost, "/api/login",
		strings.NewReader(`{"username":"admin","password":"`+testAdminPassword+`"}`))
	login.Header.Set("Content-Type", "application/json")
	login.Header.Set("X-Requested-With", "ts3panel")
	loginRec := httptest.NewRecorder()
	handler.ServeHTTP(loginRec, login)
	if loginRec.Code != http.StatusOK {
		t.Fatalf("login status = %d, body=%s", loginRec.Code, loginRec.Body.String())
	}
	var sid *http.Cookie
	for _, cookie := range loginRec.Result().Cookies() {
		if cookie.Name == sessionCookieName {
			sid = cookie
		}
	}
	if sid == nil {
		t.Fatal("login did not set the sid cookie")
	}

	do := func(method, target, body string, csrf bool) *httptest.ResponseRecorder {
		var reader *strings.Reader
		if body == "" {
			reader = strings.NewReader("")
		} else {
			reader = strings.NewReader(body)
		}
		req := httptest.NewRequest(method, target, reader)
		req.Header.Set("Content-Type", "application/json")
		if csrf {
			req.Header.Set("X-Requested-With", "ts3panel")
		}
		req.AddCookie(sid)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}

	// Missing CSRF header on a write must be rejected before the handler runs.
	if rec := do(http.MethodPost, "/api/instances/ts1/domain", `{}`, false); rec.Code != http.StatusForbidden {
		t.Fatalf("missing CSRF status = %d, body=%s", rec.Code, rec.Body.String())
	}

	bind := do(http.MethodPost, "/api/instances/ts1/domain",
		`{"subdomain":"ts1","provider_id":"cf-main"}`, true)
	if bind.Code != http.StatusOK {
		t.Fatalf("bind status = %d, body=%s", bind.Code, bind.Body.String())
	}
	resp := decodeBind(t, bind)
	if len(resp.Manual) != 2 || resp.DNSStatus != model.DNSStatusNone {
		t.Fatalf("bind response = %+v", resp)
	}

	manual := do(http.MethodGet, "/api/instances/ts1/domain/manual", "", true)
	if manual.Code != http.StatusOK {
		t.Fatalf("manual status = %d, body=%s", manual.Code, manual.Body.String())
	}
	var manualResp manualDomainResponse
	if err := json.Unmarshal(manual.Body.Bytes(), &manualResp); err != nil {
		t.Fatalf("decode manual: %v", err)
	}
	if len(manualResp.Records) != 2 || !strings.Contains(manualResp.Text, "_ts3._udp.ts1") {
		t.Fatalf("manual response = %+v", manualResp)
	}

	// Unauthenticated read is rejected by the auth middleware.
	anon := httptest.NewRequest(http.MethodGet, "/api/providers", nil)
	anonRec := httptest.NewRecorder()
	handler.ServeHTTP(anonRec, anon)
	if anonRec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous providers status = %d", anonRec.Code)
	}
}
