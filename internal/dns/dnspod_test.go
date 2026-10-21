package dns

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"sync"
	"testing"
	"time"
)

type dpStored struct {
	ID         int64
	SubDomain  string
	RecordType string
	RecordLine string
	Value      string
	TTL        float64
}

type dpFake struct {
	mu          sync.Mutex
	records     map[int64]*dpStored
	nextID      int64
	forceCode   string
	actions     []string
	authOK      bool
	authChecked bool
}

func newDPFake() *dpFake {
	return &dpFake{records: map[int64]*dpStored{}, nextID: 100}
}

func (f *dpFake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()

	action := r.Header.Get("X-TC-Action")
	f.actions = append(f.actions, action)
	body, _ := io.ReadAll(r.Body)
	_ = r.Body.Close()
	f.authChecked = true
	f.authOK = r.Header.Get("Authorization") == dnspodAuthorization("AKIDtest", "testkey", r.Host, action, body, 1600000000)

	if f.forceCode != "" {
		writeDPJSON(w, map[string]any{"Response": map[string]any{
			"RequestId": "req", "Error": map[string]any{"Code": f.forceCode, "Message": "synthetic"},
		}})
		return
	}

	var payload map[string]any
	_ = json.Unmarshal(body, &payload)
	switch action {
	case "DescribeDomainList":
		writeDPJSON(w, map[string]any{"Response": map[string]any{
			"RequestId":  "req",
			"DomainList": []map[string]any{{"ID": 123, "Name": "example.com"}},
		}})
	case "DescribeRecordList":
		sub, _ := payload["Subdomain"].(string)
		recordType, _ := payload["RecordType"].(string)
		var out []map[string]any
		ids := make([]int64, 0, len(f.records))
		for id := range f.records {
			ids = append(ids, id)
		}
		sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
		for _, id := range ids {
			rec := f.records[id]
			if sub != "" && rec.SubDomain != sub {
				continue
			}
			if recordType != "" && rec.RecordType != recordType {
				continue
			}
			out = append(out, map[string]any{
				"RecordId": rec.ID, "Name": rec.SubDomain, "Type": rec.RecordType, "Value": rec.Value,
			})
		}
		writeDPJSON(w, map[string]any{"Response": map[string]any{
			"RequestId": "req", "RecordList": out, "TotalCount": len(out),
		}})
	case "CreateRecord":
		f.nextID++
		rec := &dpStored{
			ID: f.nextID, SubDomain: str(payload["SubDomain"]), RecordType: str(payload["RecordType"]),
			RecordLine: str(payload["RecordLine"]), Value: str(payload["Value"]), TTL: num(payload["TTL"]),
		}
		f.records[rec.ID] = rec
		writeDPJSON(w, map[string]any{"Response": map[string]any{"RequestId": "req", "RecordId": rec.ID}})
	case "ModifyRecord":
		id := int64(num(payload["RecordId"]))
		rec, ok := f.records[id]
		if !ok {
			writeDPJSON(w, map[string]any{"Response": map[string]any{
				"RequestId": "req", "Error": map[string]any{"Code": "ResourceNotFound.NoDataOfRecord", "Message": "missing"},
			}})
			return
		}
		rec.SubDomain = str(payload["SubDomain"])
		rec.RecordType = str(payload["RecordType"])
		rec.RecordLine = str(payload["RecordLine"])
		rec.Value = str(payload["Value"])
		rec.TTL = num(payload["TTL"])
		writeDPJSON(w, map[string]any{"Response": map[string]any{"RequestId": "req", "RecordId": id}})
	case "DeleteRecord":
		id := int64(num(payload["RecordId"]))
		if _, ok := f.records[id]; !ok {
			writeDPJSON(w, map[string]any{"Response": map[string]any{
				"RequestId": "req", "Error": map[string]any{"Code": "ResourceNotFound.NoDataOfRecord", "Message": "missing"},
			}})
			return
		}
		delete(f.records, id)
		writeDPJSON(w, map[string]any{"Response": map[string]any{"RequestId": "req"}})
	default:
		writeDPJSON(w, map[string]any{"Response": map[string]any{
			"RequestId": "req", "Error": map[string]any{"Code": "InvalidAction", "Message": action},
		}})
	}
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

func num(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case json.Number:
		f, _ := n.Float64()
		return f
	case string:
		f, _ := strconv.ParseFloat(n, 64)
		return f
	default:
		return 0
	}
}

func writeDPJSON(w http.ResponseWriter, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(body)
}

func (f *dpFake) list() []*dpStored {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]*dpStored, 0, len(f.records))
	for _, rec := range f.records {
		out = append(out, rec)
	}
	return out
}

func newDPTestProvider(t *testing.T, fake *dpFake) *dnspod {
	t.Helper()
	server := httptest.NewServer(fake)
	t.Cleanup(server.Close)
	fixed := time.Unix(1600000000, 0).UTC()
	return newDNSPod(server.URL, "AKIDtest", "testkey", server.Client(), func() time.Time { return fixed })
}

func TestDNSPodAuthorizationGolden(t *testing.T) {
	payload := []byte(`{"Domain":"example.com","RecordLine":"默认","RecordType":"SRV","SubDomain":"_ts3._udp.ts1","TTL":300,"Value":"0 5 9987 ts1.example.com"}`)
	got := dnspodAuthorization("AKIDtest", "testkey", "dnspod.tencentcloudapi.com", "CreateRecord", payload, 1600000000)
	want := "TC3-HMAC-SHA256 Credential=AKIDtest/2020-09-13/dnspod/tc3_request, " +
		"SignedHeaders=content-type;host;x-tc-action, " +
		"Signature=ed56de180008b6197ea3c5a5cfdee63e10e640b76641b4130907de21d71cde51"
	if got != want {
		t.Fatalf("authorization =\n%s\nwant\n%s", got, want)
	}
}

func TestDNSPodAIdempotentAndSigned(t *testing.T) {
	fake := newDPFake()
	dp := newDPTestProvider(t, fake)
	ctx := context.Background()

	first, err := dp.UpsertA(ctx, "example.com", "ts1.example.com", "1.2.3.4", 300)
	if err != nil {
		t.Fatalf("first UpsertA: %v", err)
	}
	second, err := dp.UpsertA(ctx, "example.com", "ts1.example.com", "1.2.3.4", 300)
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
	if rec.SubDomain != "ts1" || rec.RecordType != "A" || rec.Value != "1.2.3.4" {
		t.Fatalf("stored A = %+v", rec)
	}
	if rec.RecordLine != "默认" {
		t.Fatalf("RecordLine = %q, want 默认", rec.RecordLine)
	}
	if !fake.authChecked || !fake.authOK {
		t.Fatal("request Authorization header does not match a fresh TC3 signature over the sent body")
	}
}

func TestDNSPodSRVValueExact(t *testing.T) {
	fake := newDPFake()
	dp := newDPTestProvider(t, fake)
	if _, err := dp.UpsertSRV(context.Background(), "example.com", "ts1.example.com", 9987, 300); err != nil {
		t.Fatalf("UpsertSRV: %v", err)
	}
	records := fake.list()
	if len(records) != 1 {
		t.Fatalf("records = %d", len(records))
	}
	rec := records[0]
	if rec.SubDomain != "_ts3._udp.ts1" || rec.RecordType != "SRV" {
		t.Fatalf("stored SRV = %+v", rec)
	}
	if rec.Value != "0 5 9987 ts1.example.com" {
		t.Fatalf("SRV value = %q", rec.Value)
	}
}

func TestDNSPodErrorMapping(t *testing.T) {
	cases := []struct {
		vendor string
		want   string
	}{
		{"AuthFailure.SignatureFailure", CodeAuth},
		{"UnauthorizedOperation", CodePerm},
		{"ResourceNotFound.NoDataOfDomain", CodeZone},
		{"RequestLimitExceeded", CodeRate},
		{"InternalError.Something", CodeUnknown},
	}
	for _, tc := range cases {
		fake := newDPFake()
		fake.forceCode = tc.vendor
		dp := newDPTestProvider(t, fake)
		_, err := dp.Validate(context.Background())
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

func TestDNSPodValidateAndDeleteMissing(t *testing.T) {
	fake := newDPFake()
	dp := newDPTestProvider(t, fake)
	zones, err := dp.Validate(context.Background())
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if len(zones) != 1 || zones[0].Name != "example.com" || zones[0].ID != "123" {
		t.Fatalf("zones = %+v", zones)
	}
	if err := dp.DeleteRecord(context.Background(), "example.com", "999"); err != nil {
		t.Fatalf("DeleteRecord missing: %v", err)
	}
}
