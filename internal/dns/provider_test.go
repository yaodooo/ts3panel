package dns

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestPrefixFor(t *testing.T) {
	cases := []struct {
		zone, fqdn, want string
		wantErr          bool
	}{
		{zone: "example.com", fqdn: "ts1.example.com", want: "ts1"},
		{zone: "Example.COM.", fqdn: "TS1.Example.com.", want: "ts1"},
		{zone: "example.com", fqdn: "a.b.example.com", want: "a.b"},
		{zone: "example.com", fqdn: "example.com", wantErr: true},
		{zone: "example.com", fqdn: "ts1.other.com", wantErr: true},
		{zone: "example.com", fqdn: "notexample.com", wantErr: true},
		{zone: "", fqdn: "ts1.example.com", wantErr: true},
	}
	for _, tc := range cases {
		got, err := PrefixFor(tc.zone, tc.fqdn)
		if tc.wantErr {
			if err == nil {
				t.Errorf("PrefixFor(%q,%q) = %q, want error", tc.zone, tc.fqdn, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("PrefixFor(%q,%q) error: %v", tc.zone, tc.fqdn, err)
			continue
		}
		if got != tc.want {
			t.Errorf("PrefixFor(%q,%q) = %q, want %q", tc.zone, tc.fqdn, got, tc.want)
		}
	}
}

func TestSRVValueAndHostExact(t *testing.T) {
	if got := SRVValue(9987, "TS1.Example.com."); got != "0 5 9987 ts1.example.com" {
		t.Fatalf("SRVValue = %q", got)
	}
	if got := RecordHost("ts1"); got != "_ts3._udp.ts1" {
		t.Fatalf("RecordHost = %q", got)
	}
	if got := SRVValue(10023, "ts1.example.com"); got != "0 5 10023 ts1.example.com" {
		t.Fatalf("SRVValue with high port = %q", got)
	}
}

func TestManualRecordsExact(t *testing.T) {
	records, err := ManualRecords("example.com", "ts1.example.com", "1.2.3.4", 9987, 300)
	if err != nil {
		t.Fatalf("ManualRecords: %v", err)
	}
	if len(records) != 2 {
		t.Fatalf("len = %d, want 2", len(records))
	}
	wantA := ManualRecord{Type: "A", Host: "ts1", Value: "1.2.3.4", TTL: 300}
	if records[0] != wantA {
		t.Fatalf("A = %+v, want %+v", records[0], wantA)
	}
	wantSRV := ManualRecord{Type: "SRV", Host: "_ts3._udp.ts1", Value: "0 5 9987 ts1.example.com", TTL: 300}
	if records[1] != wantSRV {
		t.Fatalf("SRV = %+v, want %+v", records[1], wantSRV)
	}
}

func TestProviderErrorCodes(t *testing.T) {
	if CodeOf(nil) != CodeUnknown {
		t.Fatal("nil error must map to E_DNS_UNKNOWN")
	}
	err := Errorf(CodeAuth, "bad token")
	if CodeOf(err) != CodeAuth {
		t.Fatalf("CodeOf = %q", CodeOf(err))
	}
	if !strings.Contains(err.Error(), CodeAuth) || !strings.Contains(err.Error(), "bad token") {
		t.Fatalf("unexpected error text %q", err.Error())
	}
	wrapped := &Error{Code: CodeRate, VendorCode: "Throttling"}
	if VendorCodeOf(wrapped) != "Throttling" {
		t.Fatalf("VendorCodeOf = %q", VendorCodeOf(wrapped))
	}
}

func TestDetectPublicIPFailureIsGeneric(t *testing.T) {
	client := &http.Client{
		Timeout: time.Second,
		Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return nil, errors.New("network unreachable")
		}),
	}
	if ip, err := DetectPublicIP(context.Background(), client); err == nil || ip != "" {
		t.Fatalf("DetectPublicIP = %q, %v; want failure", ip, err)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
