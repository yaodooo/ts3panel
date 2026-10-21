package dns

import (
	"context"
	"fmt"
	"net"
	"sort"
	"strings"
	"time"
)

// SRVLookupFunc mirrors net.Resolver.LookupSRV, so tests can substitute a fake
// resolver without any network access.
type SRVLookupFunc func(ctx context.Context, service, proto, name string) (string, []*net.SRV, error)

// HostLookupFunc mirrors net.Resolver.LookupHost.
type HostLookupFunc func(ctx context.Context, host string) ([]string, error)

const (
	// VerifyInterval and VerifyTimeout follow spec §13: poll every 5 seconds
	// for at most 180 seconds. Both are configurable for tests.
	VerifyInterval = 5 * time.Second
	VerifyTimeout  = 180 * time.Second
)

// Verifier checks DNS propagation with the system resolver. It never trusts an
// API write response as proof of propagation (spec §13 hard constraint).
type Verifier struct {
	LookupSRV  SRVLookupFunc
	LookupHost HostLookupFunc
	Now        func() time.Time
	Interval   time.Duration
	Timeout    time.Duration
}

// NewVerifier returns a Verifier backed by net.DefaultResolver.
func NewVerifier() *Verifier {
	resolver := net.DefaultResolver
	return &Verifier{
		LookupSRV: func(ctx context.Context, service, proto, name string) (string, []*net.SRV, error) {
			return resolver.LookupSRV(ctx, service, proto, name)
		},
		LookupHost: func(ctx context.Context, host string) ([]string, error) {
			return resolver.LookupHost(ctx, host)
		},
		Now:      time.Now,
		Interval: VerifyInterval,
		Timeout:  VerifyTimeout,
	}
}

// CheckResult is one verification attempt plus the details the UI needs to
// explain a partial result.
type CheckResult struct {
	FQDN        string    `json:"fqdn"`
	Port        int       `json:"port"`
	PublicIP    string    `json:"public_ip,omitempty"`
	AVerified   bool      `json:"a_verified"`
	SRVVerified bool      `json:"srv_verified"`
	Verified    bool      `json:"verified"`
	ARecords    []string  `json:"a_records,omitempty"`
	SRVTarget   string    `json:"srv_target,omitempty"`
	SRVPort     int       `json:"srv_port,omitempty"`
	AError      string    `json:"a_error,omitempty"`
	SRVError    string    `json:"srv_error,omitempty"`
	Message     string    `json:"message"`
	Attempts    int       `json:"attempts"`
	ElapsedMS   int64     `json:"elapsed_ms"`
	CheckedAt   time.Time `json:"checked_at"`
}

// Check performs one A + SRV lookup round.
func (v *Verifier) Check(ctx context.Context, fqdn string, port int, publicIP string) CheckResult {
	fqdn = normalizeName(fqdn)
	res := CheckResult{
		FQDN:     fqdn,
		Port:     port,
		PublicIP: strings.TrimSpace(publicIP),
		Attempts: 1,
	}
	nowFn := v.Now
	if nowFn == nil {
		nowFn = time.Now
	}
	res.CheckedAt = nowFn().UTC()

	if v.LookupHost != nil {
		hosts, err := v.LookupHost(ctx, fqdn)
		switch {
		case err != nil:
			res.AError = resolverError("A", err)
		default:
			normalized := make([]string, 0, len(hosts))
			seen := make(map[string]bool, len(hosts))
			for _, host := range hosts {
				host = strings.TrimSpace(host)
				if host == "" {
					continue
				}
				if !seen[host] {
					seen[host] = true
					normalized = append(normalized, host)
				}
			}
			sort.Strings(normalized)
			res.ARecords = normalized
			if publicIP == "" {
				// Without a configured public IP the strongest truthful check
				// is "the name resolves"; the operator was asked to set the IP.
				res.AVerified = len(normalized) > 0
			} else {
				want := strings.TrimSpace(publicIP)
				for _, host := range normalized {
					if ipEqual(host, want) {
						res.AVerified = true
						break
					}
				}
			}
		}
	}

	if v.LookupSRV != nil {
		_, records, err := v.LookupSRV(ctx, "ts3", "udp", fqdn)
		switch {
		case err != nil:
			res.SRVError = resolverError("SRV", err)
		default:
			for _, rec := range records {
				if rec == nil {
					continue
				}
				target := normalizeName(rec.Target)
				if target == fqdn && int(rec.Port) == port {
					res.SRVVerified = true
					res.SRVTarget = target
					res.SRVPort = int(rec.Port)
					break
				}
				if res.SRVTarget == "" {
					res.SRVTarget = target
					res.SRVPort = int(rec.Port)
				}
			}
		}
	}

	res.Verified = res.AVerified && res.SRVVerified
	res.Message = verificationMessage(res)
	return res
}

// Wait polls Check every Interval until both records match or Timeout expires.
// A timeout is not an error: the result simply reports what was observed, so
// the UI can say "still propagating" instead of claiming the port is broken.
func (v *Verifier) Wait(ctx context.Context, fqdn string, port int, publicIP string) CheckResult {
	interval := v.Interval
	if interval <= 0 {
		interval = VerifyInterval
	}
	timeout := v.Timeout
	if timeout <= 0 {
		timeout = VerifyTimeout
	}
	start := time.Now()
	deadline := start.Add(timeout)

	var last CheckResult
	for attempt := 1; ; attempt++ {
		last = v.Check(ctx, fqdn, port, publicIP)
		last.Attempts = attempt
		if last.Verified {
			last.ElapsedMS = time.Since(start).Milliseconds()
			return last
		}
		if !time.Now().Add(interval).Before(deadline) {
			last.ElapsedMS = time.Since(start).Milliseconds()
			last.Message = last.Message + " 记录可能仍在传播，请稍后重试。"
			return last
		}
		select {
		case <-ctx.Done():
			last.ElapsedMS = time.Since(start).Milliseconds()
			last.Message = last.Message + " 验证被取消。"
			return last
		case <-time.After(interval):
		}
	}
}

func verificationMessage(res CheckResult) string {
	switch {
	case res.Verified:
		return "A 记录与 SRV 记录均已生效。"
	case res.AVerified && !res.SRVVerified:
		return "A 记录已生效，SRV 记录尚未生效。"
	case !res.AVerified && res.SRVVerified:
		return "SRV 记录已生效，A 记录尚未生效或与面板记录的公网 IP 不一致。"
	default:
		return "A 记录与 SRV 记录均未生效。"
	}
}

func resolverError(kind string, err error) string {
	if err == nil {
		return ""
	}
	return fmt.Sprintf("%s 查询失败：%v", kind, err)
}

// ipEqual compares two textual IP addresses (IPv4 or IPv6) semantically.
func ipEqual(a, b string) bool {
	ipA := net.ParseIP(strings.TrimSpace(a))
	ipB := net.ParseIP(strings.TrimSpace(b))
	if ipA != nil && ipB != nil {
		return ipA.Equal(ipB)
	}
	return strings.EqualFold(strings.TrimSpace(a), strings.TrimSpace(b))
}
