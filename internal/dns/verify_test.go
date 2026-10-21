package dns

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestVerifierMatch(t *testing.T) {
	v := &Verifier{
		LookupHost: func(context.Context, string) ([]string, error) {
			return []string{"1.2.3.4"}, nil
		},
		LookupSRV: func(context.Context, string, string, string) (string, []*net.SRV, error) {
			return "", []*net.SRV{{Target: "ts1.example.com.", Port: 9987}}, nil
		},
		Now: time.Now, Interval: time.Millisecond, Timeout: 5 * time.Millisecond,
	}
	result := v.Check(context.Background(), "ts1.example.com", 9987, "1.2.3.4")
	if !result.Verified || !result.AVerified || !result.SRVVerified {
		t.Fatalf("result = %+v, want fully verified", result)
	}
	if result.SRVTarget != "ts1.example.com" || result.SRVPort != 9987 {
		t.Fatalf("SRV detail = %q:%d", result.SRVTarget, result.SRVPort)
	}
}

func TestVerifierMismatch(t *testing.T) {
	v := &Verifier{
		LookupHost: func(context.Context, string) ([]string, error) {
			return []string{"5.6.7.8"}, nil
		},
		LookupSRV: func(context.Context, string, string, string) (string, []*net.SRV, error) {
			return "", []*net.SRV{{Target: "ts1.example.com.", Port: 10000}}, nil
		},
		Now: time.Now,
	}
	result := v.Check(context.Background(), "ts1.example.com", 9987, "1.2.3.4")
	if result.Verified || result.AVerified || result.SRVVerified {
		t.Fatalf("result = %+v, want all mismatches", result)
	}
	if result.SRVPort != 10000 {
		t.Fatalf("SRVPort = %d", result.SRVPort)
	}
	if !strings.Contains(result.Message, "均未生效") {
		t.Fatalf("message = %q", result.Message)
	}
}

func TestVerifierLookupErrorsAreNotProofOfAbsence(t *testing.T) {
	v := &Verifier{
		LookupHost: func(_ context.Context, host string) ([]string, error) {
			return nil, &net.DNSError{Err: "timeout", Name: host, IsTimeout: true}
		},
		LookupSRV: func(_ context.Context, _, _, name string) (string, []*net.SRV, error) {
			return "", nil, &net.DNSError{Err: "no such host", Name: name, IsNotFound: true}
		},
		Now: time.Now,
	}
	result := v.Check(context.Background(), "ts1.example.com", 9987, "1.2.3.4")
	if result.Verified {
		t.Fatal("lookup errors must not yield verified=true")
	}
	if result.AError == "" || result.SRVError == "" {
		t.Fatalf("errors not reported: %+v", result)
	}
}

func TestVerifierWaitRetriesUntilPropagated(t *testing.T) {
	var calls int32
	v := &Verifier{
		LookupHost: func(context.Context, string) ([]string, error) {
			if atomic.AddInt32(&calls, 1) == 1 {
				return nil, &net.DNSError{Err: "not found", IsNotFound: true}
			}
			return []string{"1.2.3.4"}, nil
		},
		LookupSRV: func(context.Context, string, string, string) (string, []*net.SRV, error) {
			return "", []*net.SRV{{Target: "ts1.example.com.", Port: 9987}}, nil
		},
		Now: time.Now, Interval: time.Millisecond, Timeout: 200 * time.Millisecond,
	}
	result := v.Wait(context.Background(), "ts1.example.com", 9987, "1.2.3.4")
	if !result.Verified {
		t.Fatalf("result = %+v, want verified after retry", result)
	}
	if result.Attempts != 2 {
		t.Fatalf("attempts = %d, want 2", result.Attempts)
	}
}

func TestVerifierWaitTimeoutKeepsPropagationWording(t *testing.T) {
	v := &Verifier{
		LookupHost: func(context.Context, string) ([]string, error) {
			return nil, &net.DNSError{Err: "not found", IsNotFound: true}
		},
		LookupSRV: func(context.Context, string, string, string) (string, []*net.SRV, error) {
			return "", nil, &net.DNSError{Err: "not found", IsNotFound: true}
		},
		Now: time.Now, Interval: time.Millisecond, Timeout: 4 * time.Millisecond,
	}
	result := v.Wait(context.Background(), "ts1.example.com", 9987, "1.2.3.4")
	if result.Verified {
		t.Fatal("timed out verification reported verified")
	}
	if !strings.Contains(result.Message, "仍在传播") {
		t.Fatalf("message = %q, want propagation wording", result.Message)
	}
	if result.Attempts < 2 {
		t.Fatalf("attempts = %d, want at least 2", result.Attempts)
	}
}

func TestVerifierContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan CheckResult, 1)
	v := &Verifier{
		LookupHost: func(context.Context, string) ([]string, error) {
			return nil, errors.New("nope")
		},
		LookupSRV: func(context.Context, string, string, string) (string, []*net.SRV, error) {
			return "", nil, errors.New("nope")
		},
		Now: time.Now, Interval: 500 * time.Millisecond, Timeout: 10 * time.Second,
	}
	go func() { done <- v.Wait(ctx, "ts1.example.com", 9987, "1.2.3.4") }()
	time.Sleep(10 * time.Millisecond)
	cancel()
	select {
	case result := <-done:
		if !strings.Contains(result.Message, "取消") {
			t.Fatalf("message = %q", result.Message)
		}
	case <-time.After(time.Second):
		t.Fatal("Wait ignored context cancellation")
	}
}
