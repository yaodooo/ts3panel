package dns

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

type cfStored struct {
	ID   string
	Type string
	Name string
	Body map[string]any
}

// cfFake is a minimal Cloudflare DNS API mock: zone lookup, record list,
// create/update/delete, and injectable create errors (81057 / 9003).
type cfFake struct {
	mu         sync.Mutex
	zones      []map[string]string
	records    map[string]*cfStored
	nextID     int
	postErrors []int
	putErrors  []int
	requests   []string
}

func newCFFake() *cfFake {
	return &cfFake{
		zones:   []map[string]string{{"id": "z1", "name": "example.com"}},
		records: map[string]*cfStored{},
	}
}

func (f *cfFake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, r.Method+" "+r.URL.RequestURI())

	switch {
	case r.URL.Path == "/client/v4/zones":
		f.writeJSON(w, http.StatusOK, map[string]any{"success": true, "result": f.zones})
		return
	case strings.HasPrefix(r.URL.Path, "/client/v4/zones/z1/dns_records"):
		f.handleRecords(w, r)
		return
	default:
		f.writeJSON(w, http.StatusNotFound, map[string]any{"success": false,
			"errors": []map[string]any{{"code": 7003, "message": "not found"}}})
	}
}

func (f *cfFake) handleRecords(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/client/v4/zones/z1/dns_records/")
	switch r.Method {
	case http.MethodGet:
		wantType := r.URL.Query().Get("type")
		wantName := r.URL.Query().Get("name")
		result := make([]map[string]any, 0)
		for _, rec := range f.records {
			if wantType != "" && !strings.EqualFold(rec.Type, wantType) {
				continue
			}
			if wantName != "" && !strings.EqualFold(rec.Name, wantName) {
				continue
			}
			result = append(result, map[string]any{"id": rec.ID, "type": rec.Type, "name": rec.Name})
		}
		f.writeJSON(w, http.StatusOK, map[string]any{"success": true, "result": result})
	case http.MethodPost:
		if len(f.postErrors) > 0 {
			code := f.postErrors[0]
			f.postErrors = f.postErrors[1:]
			if code == 81057 {
				// Simulate the race the real API reports: the record appeared
				// between the lookup and the create.
				var body map[string]any
				_ = json.NewDecoder(r.Body).Decode(&body)
				recordType, _ := body["type"].(string)
				name, _ := body["name"].(string)
				f.records["concurrent"] = &cfStored{ID: "concurrent", Type: recordType, Name: name, Body: body}
			}
			f.reject(w, code)
			return
		}
		f.store(w, r, "")
	case http.MethodPut:
		if len(f.putErrors) > 0 {
			code := f.putErrors[0]
			f.putErrors = f.putErrors[1:]
			f.reject(w, code)
			return
		}
		if _, ok := f.records[id]; !ok {
			f.writeJSON(w, http.StatusNotFound, map[string]any{"success": false,
				"errors": []map[string]any{{"code": 81044, "message": "record not found"}}})
			return
		}
		f.store(w, r, id)
	case http.MethodDelete:
		if _, ok := f.records[id]; !ok {
			f.writeJSON(w, http.StatusNotFound, map[string]any{"success": false,
				"errors": []map[string]any{{"code": 81044, "message": "record not found"}}})
			return
		}
		delete(f.records, id)
		f.writeJSON(w, http.StatusOK, map[string]any{"success": true, "result": map[string]any{"id": id}})
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (f *cfFake) reject(w http.ResponseWriter, code int) {
	f.writeJSON(w, http.StatusBadRequest, map[string]any{"success": false,
		"errors": []map[string]any{{"code": code, "message": fmt.Sprintf("synthetic error %d", code)}}})
}

func (f *cfFake) store(w http.ResponseWriter, r *http.Request, existing string) {
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	recID := existing
	if recID == "" {
		f.nextID++
		recID = fmt.Sprintf("rec%d", f.nextID)
	}
	recordType, _ := body["type"].(string)
	name, _ := body["name"].(string)
	f.records[recID] = &cfStored{ID: recID, Type: recordType, Name: name, Body: body}
	f.writeJSON(w, http.StatusOK, map[string]any{"success": true, "result": map[string]any{"id": recID}})
}

func (f *cfFake) writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func (f *cfFake) ofType(typ string) []*cfStored {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*cfStored
	for _, rec := range f.records {
		if rec.Type == typ {
			out = append(out, rec)
		}
	}
	return out
}

func newCFTestProvider(t *testing.T, fake *cfFake) (*httptest.Server, *cloudflare) {
	t.Helper()
	server := httptest.NewServer(fake)
	t.Cleanup(server.Close)
	return server, newCloudflare(server.URL+"/client/v4", "test-token", "example.com", "", server.Client())
}

func TestCloudflareValidateAndAIdempotent(t *testing.T) {
	fake := newCFFake()
	_, cf := newCFTestProvider(t, fake)

	zones, err := cf.Validate(context.Background())
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if len(zones) != 1 || zones[0].ID != "z1" || zones[0].Name != "example.com" {
		t.Fatalf("zones = %+v", zones)
	}

	first, err := cf.UpsertA(context.Background(), "example.com", "ts1.example.com", "1.2.3.4", 300)
	if err != nil {
		t.Fatalf("first UpsertA: %v", err)
	}
	second, err := cf.UpsertA(context.Background(), "example.com", "ts1.example.com", "1.2.3.4", 300)
	if err != nil {
		t.Fatalf("second UpsertA: %v", err)
	}
	if first != second {
		t.Fatalf("record id changed: %q -> %q (not idempotent)", first, second)
	}

	stored := fake.ofType("A")
	if len(stored) != 1 {
		t.Fatalf("A records = %d, want 1", len(stored))
	}
	body := stored[0].Body
	if body["proxied"] != false {
		t.Fatalf("proxied = %v, want false", body["proxied"])
	}
	if body["ttl"] != float64(300) {
		t.Fatalf("ttl = %v, want 300", body["ttl"])
	}
	if body["name"] != "ts1.example.com" || body["content"] != "1.2.3.4" {
		t.Fatalf("body = %v", body)
	}
	if body["comment"] != "ts3panel:ts1" {
		t.Fatalf("comment = %v", body["comment"])
	}
}

func TestCloudflareSRVUsesDataObject(t *testing.T) {
	fake := newCFFake()
	_, cf := newCFTestProvider(t, fake)

	if _, err := cf.UpsertSRV(context.Background(), "example.com", "ts1.example.com", 9987, 300); err != nil {
		t.Fatalf("UpsertSRV: %v", err)
	}
	if _, err := cf.UpsertSRV(context.Background(), "example.com", "ts1.example.com", 9987, 300); err != nil {
		t.Fatalf("second UpsertSRV: %v", err)
	}
	stored := fake.ofType("SRV")
	if len(stored) != 1 {
		t.Fatalf("SRV records = %d, want 1", len(stored))
	}
	body := stored[0].Body
	if body["name"] != "_ts3._udp.ts1.example.com" {
		t.Fatalf("name = %v", body["name"])
	}
	if body["type"] != "SRV" {
		t.Fatalf("type = %v", body["type"])
	}
	data, ok := body["data"].(map[string]any)
	if !ok {
		t.Fatalf("data is %T, want object", body["data"])
	}
	want := map[string]any{
		"service": "_ts3", "proto": "_udp", "name": "ts1",
		"priority": float64(0), "weight": float64(5),
		"port": float64(9987), "target": "ts1.example.com",
	}
	for key, value := range want {
		if data[key] != value {
			t.Fatalf("data[%q] = %v, want %v", key, data[key], value)
		}
	}
	if _, hasString := body["content"]; hasString {
		t.Fatalf("SRV body must not use a content string: %v", body)
	}
}

func TestCloudflareRecoversFrom81057(t *testing.T) {
	fake := newCFFake()
	fake.postErrors = []int{81057}
	_, cf := newCFTestProvider(t, fake)

	id, err := cf.UpsertA(context.Background(), "example.com", "ts1.example.com", "1.2.3.4", 300)
	if err != nil {
		t.Fatalf("UpsertA after 81057: %v", err)
	}
	if id == "" {
		t.Fatal("empty record id")
	}
	if len(fake.ofType("A")) != 1 {
		t.Fatalf("A records = %d, want 1", len(fake.ofType("A")))
	}
}

func TestCloudflareTTLFallsBackTo1(t *testing.T) {
	fake := newCFFake()
	fake.postErrors = []int{9003}
	_, cf := newCFTestProvider(t, fake)

	if _, err := cf.UpsertA(context.Background(), "example.com", "ts1.example.com", "1.2.3.4", 300); err != nil {
		t.Fatalf("UpsertA with TTL fallback: %v", err)
	}
	stored := fake.ofType("A")
	if len(stored) != 1 {
		t.Fatalf("A records = %d", len(stored))
	}
	if stored[0].Body["ttl"] != float64(1) {
		t.Fatalf("ttl = %v, want 1 after 9003", stored[0].Body["ttl"])
	}
}

func TestCloudflareTTLFallsBackOnUpdate(t *testing.T) {
	fake := newCFFake()
	_, cf := newCFTestProvider(t, fake)
	ctx := context.Background()
	if _, err := cf.UpsertA(ctx, "example.com", "ts1.example.com", "1.2.3.4", 300); err != nil {
		t.Fatalf("initial UpsertA: %v", err)
	}

	fake.mu.Lock()
	fake.putErrors = []int{9003}
	fake.mu.Unlock()
	if _, err := cf.UpsertA(ctx, "example.com", "ts1.example.com", "1.2.3.5", 300); err != nil {
		t.Fatalf("UpsertA with update TTL fallback: %v", err)
	}
	stored := fake.ofType("A")
	if len(stored) != 1 {
		t.Fatalf("A records = %d", len(stored))
	}
	if stored[0].Body["ttl"] != float64(1) {
		t.Fatalf("ttl = %v, want 1 after 9003 on update", stored[0].Body["ttl"])
	}
	if stored[0].Body["content"] != "1.2.3.5" {
		t.Fatalf("content = %v, want the retried update", stored[0].Body["content"])
	}
}

func TestCloudflareAuthErrorMapping(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"success":false,"errors":[{"code":9109,"message":"Invalid access token"}]}`))
	}))
	defer server.Close()

	cf := newCloudflare(server.URL+"/client/v4", "bad-token", "example.com", "", server.Client())
	_, err := cf.Validate(context.Background())
	if err == nil {
		t.Fatal("expected an error")
	}
	if CodeOf(err) != CodeAuth {
		t.Fatalf("CodeOf = %q, want %q (err=%v)", CodeOf(err), CodeAuth, err)
	}
}

func TestCloudflareDeleteMissingIsIdempotent(t *testing.T) {
	fake := newCFFake()
	_, cf := newCFTestProvider(t, fake)
	if err := cf.DeleteRecord(context.Background(), "example.com", "rec-missing"); err != nil {
		t.Fatalf("DeleteRecord missing: %v", err)
	}
}

func TestCloudflareLookupNotFound(t *testing.T) {
	fake := newCFFake()
	_, cf := newCFTestProvider(t, fake)
	_, err := cf.LookupA(context.Background(), "example.com", "ts1.example.com")
	if !errors.Is(err, ErrRecordNotFound) {
		t.Fatalf("err = %v, want ErrRecordNotFound", err)
	}
}
