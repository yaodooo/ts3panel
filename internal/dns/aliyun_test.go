package dns

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

type aliStored struct {
	ID    string
	RR    string
	Type  string
	Value string
	TTL   string
}

type aliFake struct {
	mu        sync.Mutex
	records   map[string]*aliStored
	next      int
	forceCode string
	actions   []string
}

func newAliFake() *aliFake {
	return &aliFake{records: map[string]*aliStored{}}
}

func (f *aliFake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	q := r.URL.Query()
	action := q.Get("Action")
	f.actions = append(f.actions, action)

	if f.forceCode != "" {
		writeAliJSON(w, http.StatusBadRequest, map[string]any{
			"Code": f.forceCode, "Message": "synthetic failure",
		})
		return
	}
	if action == "" {
		writeAliJSON(w, http.StatusBadRequest, map[string]any{"Code": "MissingParameter", "Message": "Action"})
		return
	}

	switch action {
	case "DescribeDomains":
		writeAliJSON(w, http.StatusOK, map[string]any{
			"RequestId": "req",
			"Domains": map[string]any{"Domain": []map[string]any{
				{"DomainName": "example.com", "DomainId": "d1"},
			}},
		})
	case "DescribeDomainRecords":
		wantRR := q.Get("RRKeyWord")
		wantType := q.Get("Type")
		var out []map[string]any
		for _, rec := range f.records {
			if wantRR != "" && rec.RR != wantRR {
				continue
			}
			if wantType != "" && rec.Type != wantType {
				continue
			}
			out = append(out, map[string]any{
				"RecordId": rec.ID, "RR": rec.RR, "Type": rec.Type, "Value": rec.Value,
				"TTL": mustAtoi(rec.TTL),
			})
		}
		writeAliJSON(w, http.StatusOK, map[string]any{
			"RequestId":     "req",
			"DomainRecords": map[string]any{"Record": out},
		})
	case "AddDomainRecord":
		f.next++
		id := "r" + strconv.Itoa(f.next)
		f.records[id] = &aliStored{
			ID: id, RR: q.Get("RR"), Type: q.Get("Type"),
			Value: q.Get("Value"), TTL: q.Get("TTL"),
		}
		writeAliJSON(w, http.StatusOK, map[string]any{"RequestId": "req", "RecordId": id})
	case "UpdateDomainRecord":
		id := q.Get("RecordId")
		rec, ok := f.records[id]
		if !ok {
			writeAliJSON(w, http.StatusBadRequest, map[string]any{
				"Code": "InvalidRecordId.NotFound", "Message": "missing",
			})
			return
		}
		rec.RR = q.Get("RR")
		rec.Type = q.Get("Type")
		rec.Value = q.Get("Value")
		rec.TTL = q.Get("TTL")
		writeAliJSON(w, http.StatusOK, map[string]any{"RequestId": "req", "RecordId": id})
	case "DeleteDomainRecord":
		id := q.Get("RecordId")
		if _, ok := f.records[id]; !ok {
			writeAliJSON(w, http.StatusBadRequest, map[string]any{
				"Code": "InvalidRecordId.NotFound", "Message": "missing",
			})
			return
		}
		delete(f.records, id)
		writeAliJSON(w, http.StatusOK, map[string]any{"RequestId": "req"})
	default:
		writeAliJSON(w, http.StatusBadRequest, map[string]any{"Code": "InvalidAction", "Message": action})
	}
}

func writeAliJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func mustAtoi(value string) int {
	n, err := strconv.Atoi(value)
	if err != nil {
		return 0
	}
	return n
}

func (f *aliFake) list() []*aliStored {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]*aliStored, 0, len(f.records))
	for _, rec := range f.records {
		out = append(out, rec)
	}
	return out
}

func newAliTestProvider(t *testing.T, fake *aliFake) *aliyun {
	t.Helper()
	server := httptest.NewServer(fake)
	t.Cleanup(server.Close)
	fixed := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	return newAliyun(server.URL, "testid", "testsecret", server.Client(), func() time.Time { return fixed })
}

func TestAliyunCanonicalQueryAndSignatureGolden(t *testing.T) {
	params := map[string]string{
		"AccessKeyId":      "testid",
		"Action":           "DescribeDomains",
		"Format":           "JSON",
		"SignatureMethod":  "HMAC-SHA1",
		"SignatureNonce":   "fixednonce123",
		"SignatureVersion": "1.0",
		"Timestamp":        "2020-01-01T00:00:00Z",
		"Version":          "2015-01-09",
	}
	wantCanonical := "AccessKeyId=testid&Action=DescribeDomains&Format=JSON&SignatureMethod=HMAC-SHA1" +
		"&SignatureNonce=fixednonce123&SignatureVersion=1.0&Timestamp=2020-01-01T00%3A00%3A00Z&Version=2015-01-09"
	if got := canonicalQuery(params); got != wantCanonical {
		t.Fatalf("canonicalQuery =\n%s\nwant\n%s", got, wantCanonical)
	}
	// Golden value computed independently with .NET HMACSHA1 over the
	// documented StringToSign (GET&%2F&<percent-encoded canonical query>).
	const wantSignature = "qHTAEE5WYCxN5jQIlgx2fCQQHXg="
	if got := signAliyunRPC("testsecret", "GET", params); got != wantSignature {
		t.Fatalf("signature = %q, want %q", got, wantSignature)
	}
	if got := percentEncode("a b*c~d/e+="); got != "a%20b%2Ac~d%2Fe%2B%3D" {
		t.Fatalf("percentEncode = %q", got)
	}
}

func TestTransportErrorTextStripsSignedURL(t *testing.T) {
	signed := "https://alidns.aliyuncs.com/?AccessKeyId=testid&Signature=very-secret-signature"
	err := &url.Error{Op: "Get", URL: signed, Err: errors.New("dial tcp: i/o timeout")}
	text := transportErrorText(err)
	if strings.Contains(text, "Signature") || strings.Contains(text, "AccessKeyId") {
		t.Fatalf("transport error leaked the signed query: %q", text)
	}
	if !strings.Contains(text, "timeout") {
		t.Fatalf("transport error lost the underlying cause: %q", text)
	}
}

func TestAliyunAIdempotent(t *testing.T) {
	fake := newAliFake()
	ali := newAliTestProvider(t, fake)
	ctx := context.Background()

	first, err := ali.UpsertA(ctx, "example.com", "ts1.example.com", "1.2.3.4", 300)
	if err != nil {
		t.Fatalf("first UpsertA: %v", err)
	}
	second, err := ali.UpsertA(ctx, "example.com", "ts1.example.com", "1.2.3.4", 300)
	if err != nil {
		t.Fatalf("second UpsertA: %v", err)
	}
	if first != second {
		t.Fatalf("record id changed %q -> %q", first, second)
	}
	records := fake.list()
	if len(records) != 1 {
		t.Fatalf("records = %d, want 1", len(records))
	}
	rec := records[0]
	if rec.RR != "ts1" || rec.Type != "A" || rec.Value != "1.2.3.4" || rec.TTL != "300" {
		t.Fatalf("stored A = %+v", rec)
	}
	wantActions := []string{
		"DescribeDomainRecords", "AddDomainRecord",
		"DescribeDomainRecords", "UpdateDomainRecord",
	}
	if len(fake.actions) != len(wantActions) {
		t.Fatalf("actions = %v, want %v", fake.actions, wantActions)
	}
	for i := range wantActions {
		if fake.actions[i] != wantActions[i] {
			t.Fatalf("actions = %v, want %v", fake.actions, wantActions)
		}
	}
}

func TestAliyunSRVValueExact(t *testing.T) {
	fake := newAliFake()
	ali := newAliTestProvider(t, fake)
	if _, err := ali.UpsertSRV(context.Background(), "example.com", "ts1.example.com", 9987, 300); err != nil {
		t.Fatalf("UpsertSRV: %v", err)
	}
	records := fake.list()
	if len(records) != 1 {
		t.Fatalf("records = %d", len(records))
	}
	rec := records[0]
	if rec.RR != "_ts3._udp.ts1" || rec.Type != "SRV" {
		t.Fatalf("stored SRV = %+v", rec)
	}
	if rec.Value != "0 5 9987 ts1.example.com" {
		t.Fatalf("SRV value = %q", rec.Value)
	}
}

func TestAliyunErrorMapping(t *testing.T) {
	cases := []struct {
		vendor string
		want   string
	}{
		{"InvalidAccessKeyId.NotFound", CodeAuth},
		{"SignatureDoesNotMatch", CodeAuth},
		{"Forbidden.RAM", CodePerm},
		{"DomainNotExist", CodeZone},
		{"Throttling.User", CodeRate},
		{"SomethingElse", CodeUnknown},
	}
	for _, tc := range cases {
		fake := newAliFake()
		fake.forceCode = tc.vendor
		ali := newAliTestProvider(t, fake)
		_, err := ali.Validate(context.Background())
		if err == nil {
			t.Fatalf("%s: expected error", tc.vendor)
		}
		if CodeOf(err) != tc.want {
			t.Fatalf("%s: CodeOf = %q, want %q", tc.vendor, CodeOf(err), tc.want)
		}
		if VendorCodeOf(err) != tc.vendor {
			t.Fatalf("%s: vendor code = %q", tc.vendor, VendorCodeOf(err))
		}
	}
}

func TestAliyunValidateAndDeleteMissing(t *testing.T) {
	fake := newAliFake()
	ali := newAliTestProvider(t, fake)
	zones, err := ali.Validate(context.Background())
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if len(zones) != 1 || zones[0].Name != "example.com" || zones[0].ID != "d1" {
		t.Fatalf("zones = %+v", zones)
	}
	if err := ali.DeleteRecord(context.Background(), "example.com", "r-missing"); err != nil {
		t.Fatalf("DeleteRecord missing: %v", err)
	}
}
