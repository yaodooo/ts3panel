// Package dns implements the DNS provider abstraction used for binding an
// instance subdomain: one A record and one _ts3._udp SRV record.
//
// Every provider translates vendor failures into the documented E_DNS_* codes
// (spec §12.1) and never logs or returns credential material. The four required
// methods are Validate, UpsertA, UpsertSRV and DeleteRecord. RecordFinder is an
// optional extension used for the pre-overwrite conflict check (F-14); the
// required interface itself is unchanged.
package dns

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Unified error codes from spec §12.1. CodeConflict is used by the API layer
// when a record already exists under a different owner (F-14); it is not a
// vendor error code.
const (
	CodeAuth     = "E_DNS_AUTH"
	CodePerm     = "E_DNS_PERM"
	CodeZone     = "E_DNS_ZONE"
	CodeRate     = "E_DNS_RATE"
	CodeUnknown  = "E_DNS_UNKNOWN"
	CodeConflict = "E_DNS_CONFLICT"
)

// RecordID is a vendor-side record identifier. It is never interpreted by the
// panel, only echoed back on update/delete.
type RecordID string

// Zone is one DNS zone visible to the configured credentials.
type Zone struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// Provider is the spec §12.1 contract. Implementations must be safe for
// concurrent use and must make the two Upsert methods idempotent: look up by
// full record name, update when present, create otherwise.
type Provider interface {
	// Validate checks credentials and returns the zones they can see.
	Validate(ctx context.Context) ([]Zone, error)
	// UpsertA writes (or replaces) the A record for fqdn.
	UpsertA(ctx context.Context, zone, fqdn, ip string, ttl int) (RecordID, error)
	// UpsertSRV writes (or replaces) _ts3._udp.<fqdn> → <port> <fqdn>.
	UpsertSRV(ctx context.Context, zone, fqdn string, port, ttl int) (RecordID, error)
	// DeleteRecord deletes exactly one record by ID. A missing record is not an
	// error so deletions are idempotent.
	DeleteRecord(ctx context.Context, zone string, id RecordID) error
}

// RecordFinder is implemented by the real providers so the API can detect a
// pre-existing record that the panel does not own before overwriting it (F-14).
// It is deliberately optional: the hard-constrained Provider interface stays at
// four methods.
type RecordFinder interface {
	LookupA(ctx context.Context, zone, fqdn string) (RecordID, error)
	LookupSRV(ctx context.Context, zone, fqdn string) (RecordID, error)
}

// Sentinel errors. They carry no vendor text so they are safe to match with
// errors.Is at the API boundary.
var (
	ErrRecordNotFound = errors.New("dns record not found")
	ErrManualMode     = errors.New("manual provider does not write dns records")
	ErrZoneMismatch   = errors.New("fqdn is not inside the configured zone")
)

// Error carries one of the unified E_DNS_* codes. Message is intended for the
// operator-facing hint; it never includes credentials.
type Error struct {
	Code string
	// Message is a short English explanation for logs. The Chinese message
	// shown in the API response is chosen by the API layer from Code.
	Message string
	// VendorCode keeps the raw vendor error code (for example Cloudflare 81057
	// or Aliyun "InvalidAccessKeyId.NotFound") so callers can branch on it
	// without parsing Message.
	VendorCode string
	Err        error
}

func (e *Error) Error() string {
	if e == nil {
		return "<nil>"
	}
	var b strings.Builder
	b.WriteString(e.Code)
	if e.VendorCode != "" {
		b.WriteString(" [")
		b.WriteString(e.VendorCode)
		b.WriteString("]")
	}
	if e.Message != "" {
		b.WriteString(": ")
		b.WriteString(e.Message)
	}
	if e.Err != nil {
		b.WriteString(": ")
		b.WriteString(e.Err.Error())
	}
	return b.String()
}

func (e *Error) Unwrap() error { return e.Err }

// Errorf builds a typed DNS error.
func Errorf(code, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

// WithVendor copies e and records the vendor-side code.
func (e *Error) WithVendor(vendor string) *Error {
	out := *e
	out.VendorCode = vendor
	return &out
}

// CodeOf returns the unified code of err, defaulting to CodeUnknown.
func CodeOf(err error) string {
	var e *Error
	if errors.As(err, &e) && e.Code != "" {
		return e.Code
	}
	return CodeUnknown
}

// VendorCodeOf returns the vendor code recorded on a typed DNS error.
func VendorCodeOf(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.VendorCode
	}
	return ""
}

// Options configures provider construction. All fields are optional; the zero
// value uses production endpoints. Tests inject a mock client and base URL.
type Options struct {
	Client            *http.Client
	CloudflareBaseURL string
	AliyunEndpoint    string
	DNSPodEndpoint    string
	Now               func() time.Time
}

const (
	defaultCloudflareBaseURL = "https://api.cloudflare.com/client/v4"
	// Aliyun DNS uses the fixed Hangzhou endpoint from spec §12.2.
	defaultAliyunEndpoint = "https://alidns.aliyuncs.com/"
	// DNSPod is part of the Tencent Cloud API3 family.
	defaultDNSPodEndpoint = "https://dnspod.tencentcloudapi.com/"
)

// NewProvider builds the provider described by a [[providers]] entry.
// Credential keys are exactly those documented in spec §9.2.
func NewProvider(typ string, creds map[string]string, zone, zoneID string, opts Options) (Provider, error) {
	if opts.Client == nil {
		opts.Client = &http.Client{Timeout: 15 * time.Second}
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	switch strings.ToLower(strings.TrimSpace(typ)) {
	case "cloudflare":
		token := strings.TrimSpace(creds["api_token"])
		if token == "" {
			return nil, Errorf(CodeAuth, "cloudflare api_token is missing")
		}
		base := strings.TrimSpace(opts.CloudflareBaseURL)
		if base == "" {
			base = defaultCloudflareBaseURL
		}
		return newCloudflare(base, token, zone, zoneID, opts.Client), nil
	case "aliyun":
		keyID := strings.TrimSpace(creds["access_key_id"])
		keySecret := strings.TrimSpace(creds["access_key_secret"])
		if keyID == "" || keySecret == "" {
			return nil, Errorf(CodeAuth, "aliyun access_key_id/access_key_secret are missing")
		}
		endpoint := strings.TrimSpace(opts.AliyunEndpoint)
		if endpoint == "" {
			endpoint = defaultAliyunEndpoint
		}
		return newAliyun(endpoint, keyID, keySecret, opts.Client, opts.Now), nil
	case "dnspod":
		secretID := strings.TrimSpace(creds["secret_id"])
		secretKey := strings.TrimSpace(creds["secret_key"])
		if secretID == "" || secretKey == "" {
			return nil, Errorf(CodeAuth, "dnspod secret_id/secret_key are missing")
		}
		endpoint := strings.TrimSpace(opts.DNSPodEndpoint)
		if endpoint == "" {
			endpoint = defaultDNSPodEndpoint
		}
		return newDNSPod(endpoint, secretID, secretKey, opts.Client, opts.Now), nil
	case "manual":
		return newManual(zone), nil
	default:
		return nil, Errorf(CodeUnknown, "unsupported provider type %q", typ)
	}
}

// SRVValue is the single SRV RDATA form used by Aliyun and DNSPod:
// "priority weight port target". Cloudflare uses a structured data object
// instead. Keeping this in one function is what lets unit tests pin the exact
// string for every provider (spec §12.2).
func SRVValue(port int, fqdn string) string {
	return fmt.Sprintf("0 5 %d %s", port, normalizeName(fqdn))
}

// RecordHost returns the SRV owner name without the zone suffix, matching the
// "_ts3._udp.<prefix>" form used by Aliyun and DNSPod.
func RecordHost(prefix string) string {
	return "_ts3._udp." + normalizeName(prefix)
}

// normalizeName lowercases a DNS name and strips one trailing dot.
func normalizeName(name string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(name)), ".")
}

// PrefixFor returns the subdomain prefix of fqdn inside zone. It rejects a
// fqdn that is not a strict child of the configured zone.
func PrefixFor(zone, fqdn string) (string, error) {
	z := normalizeName(zone)
	f := normalizeName(fqdn)
	if z == "" {
		return "", fmt.Errorf("%w: zone is empty", ErrZoneMismatch)
	}
	if f == z {
		return "", fmt.Errorf("%w: %q is the zone apex", ErrZoneMismatch, f)
	}
	if !strings.HasSuffix(f, "."+z) {
		return "", fmt.Errorf("%w: %q is not inside %q", ErrZoneMismatch, f, z)
	}
	prefix := strings.TrimSuffix(f, "."+z)
	if prefix == "" || strings.HasPrefix(prefix, ".") || strings.HasSuffix(prefix, ".") {
		return "", fmt.Errorf("%w: malformed subdomain %q", ErrZoneMismatch, f)
	}
	return prefix, nil
}

// ManualRecord is one line of the copyable manual DNS checklist (§12.4).
type ManualRecord struct {
	Type  string `json:"type"`
	Host  string `json:"host"`
	Value string `json:"value"`
	TTL   int    `json:"ttl"`
}

// ManualRecords builds the A + SRV checklist for a subdomain. It performs no
// network I/O and is therefore safe to call from failure paths.
func ManualRecords(zone, fqdn, ip string, port, ttl int) ([]ManualRecord, error) {
	prefix, err := PrefixFor(zone, fqdn)
	if err != nil {
		return nil, err
	}
	if ttl <= 0 {
		ttl = 300
	}
	return []ManualRecord{
		{Type: "A", Host: prefix, Value: ip, TTL: ttl},
		{Type: "SRV", Host: RecordHost(prefix), Value: SRVValue(port, fqdn), TTL: ttl},
	}, nil
}

// transportErrorText strips the URL out of net/http transport errors. The
// Aliyun RPC call carries its signature in the query string, so formatting a
// *url.Error directly would leak that signature into API hints or logs.
func transportErrorText(err error) string {
	var urlErr *url.Error
	if errors.As(err, &urlErr) && urlErr.Err != nil {
		return urlErr.Err.Error()
	}
	return err.Error()
}

// DetectPublicIP probes the documented public-IP services in order (§12.5).
// It returns an IPv4 address or a generic error; the caller decides whether to
// ask the operator to fill public_ip manually.
func DetectPublicIP(ctx context.Context, client *http.Client) (string, error) {
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Second}
	}
	endpoints := []string{"https://api.ipify.org", "https://ifconfig.me/ip"}
	var lastErr error
	for _, endpoint := range endpoints {
		reqCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, endpoint, nil)
		if err != nil {
			cancel()
			lastErr = err
			continue
		}
		req.Header.Set("Accept", "text/plain")
		req.Header.Set("User-Agent", "ts3panel/1.0")
		resp, err := client.Do(req)
		if err != nil {
			cancel()
			lastErr = err
			continue
		}
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, 256))
		_ = resp.Body.Close()
		cancel()
		if readErr != nil {
			lastErr = readErr
			continue
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			lastErr = fmt.Errorf("public ip endpoint %s returned HTTP %d", endpoint, resp.StatusCode)
			continue
		}
		ip := net.ParseIP(strings.TrimSpace(string(body)))
		if ip == nil || ip.To4() == nil {
			lastErr = fmt.Errorf("public ip endpoint %s returned an invalid address", endpoint)
			continue
		}
		return ip.To4().String(), nil
	}
	if lastErr == nil {
		lastErr = errors.New("no public ip endpoint available")
	}
	return "", fmt.Errorf("detect public ip: %w", lastErr)
}
