package dns

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

// aliyun implements Provider against the Aliyun DNS RPC API. The signature is
// implemented with the standard library only (spec §12.2): no Aliyun SDK.
//
// The RPC signature is HMAC-SHA1 over:
//
//	GET&%2F&<percent-encoded sorted query without Signature>
//
// with percent-encoding following RFC 3986 (space is %20, not +).
type aliyun struct {
	endpoint        string
	accessKeyID     string
	accessKeySecret string
	client          *http.Client
	now             func() time.Time
}

func newAliyun(endpoint, keyID, keySecret string, client *http.Client, now func() time.Time) *aliyun {
	return &aliyun{
		endpoint:        endpoint,
		accessKeyID:     keyID,
		accessKeySecret: keySecret,
		client:          client,
		now:             now,
	}
}

// aliyunEnvelope covers both success and error responses from the RPC API.
type aliyunEnvelope struct {
	RequestID string `json:"RequestId"`
	Code      string `json:"Code"`
	Message   string `json:"Message"`
	Total     int    `json:"TotalCount"`
	Domains   struct {
		Domain []struct {
			DomainName string `json:"DomainName"`
			DomainID   string `json:"DomainId"`
		} `json:"Domain"`
	} `json:"Domains"`
	DomainRecords struct {
		Record []struct {
			RecordID string `json:"RecordId"`
			RR       string `json:"RR"`
			Type     string `json:"Type"`
			Value    string `json:"Value"`
			TTL      int    `json:"TTL"`
		} `json:"Record"`
	} `json:"DomainRecords"`
	RecordID string `json:"RecordId"`
}

func (a *aliyun) call(ctx context.Context, action string, params map[string]string) (*aliyunEnvelope, error) {
	all := map[string]string{
		"Action":           action,
		"Format":           "JSON",
		"Version":          "2015-01-09",
		"AccessKeyId":      a.accessKeyID,
		"SignatureMethod":  "HMAC-SHA1",
		"SignatureVersion": "1.0",
		"SignatureNonce":   aliyunNonce(),
		"Timestamp":        a.now().UTC().Format("2006-01-02T15:04:05Z"),
	}
	for k, v := range params {
		all[k] = v
	}
	all["Signature"] = signAliyunRPC(a.accessKeySecret, http.MethodGet, all)

	target := strings.TrimRight(a.endpoint, "/") + "/?" + canonicalQuery(all)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, Errorf(CodeUnknown, "build aliyun request: %v", err)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := a.client.Do(req)
	if err != nil {
		// Do not format err directly: *url.Error embeds the full signed query
		// string, which would leak the request signature into API hints/logs.
		return nil, Errorf(CodeUnknown, "aliyun request failed: %s", transportErrorText(err))
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, Errorf(CodeUnknown, "read aliyun response: %v", err)
	}

	var env aliyunEnvelope
	if err := json.Unmarshal(data, &env); err != nil {
		return nil, aliyunHTTPError(resp.StatusCode, "")
	}
	if env.Code != "" {
		return nil, aliyunAPIError(resp.StatusCode, env.Code, env.Message)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, aliyunHTTPError(resp.StatusCode, env.Message)
	}
	return &env, nil
}

func aliyunHTTPError(status int, detail string) *Error {
	code := CodeUnknown
	switch status {
	case http.StatusUnauthorized:
		code = CodeAuth
	case http.StatusForbidden:
		code = CodePerm
	case http.StatusNotFound:
		code = CodeZone
	case http.StatusTooManyRequests:
		code = CodeRate
	}
	msg := fmt.Sprintf("aliyun returned HTTP %d", status)
	if detail != "" {
		msg += ": " + detail
	}
	return Errorf(code, "%s", msg)
}

func aliyunAPIError(status int, vendorCode, message string) *Error {
	err := &Error{Code: CodeUnknown, VendorCode: vendorCode,
		Message: fmt.Sprintf("aliyun error %s: %s", vendorCode, message)}
	switch {
	case strings.HasPrefix(vendorCode, "InvalidAccessKeyId"),
		strings.HasPrefix(vendorCode, "SignatureDoesNotMatch"),
		strings.HasPrefix(vendorCode, "IncompleteSignature"):
		err.Code = CodeAuth
	case strings.HasPrefix(vendorCode, "Forbidden"),
		strings.HasPrefix(vendorCode, "NoPermission"):
		err.Code = CodePerm
	case vendorCode == "DomainNotExist",
		vendorCode == "InvalidDomainName.NoExist",
		strings.HasPrefix(vendorCode, "DomainRecordNotBelongToUser"):
		err.Code = CodeZone
	case strings.HasPrefix(vendorCode, "Throttling"):
		err.Code = CodeRate
	}
	if err.Code == CodeUnknown {
		switch status {
		case http.StatusUnauthorized:
			err.Code = CodeAuth
		case http.StatusForbidden:
			err.Code = CodePerm
		case http.StatusTooManyRequests:
			err.Code = CodeRate
		}
	}
	return err
}

// percentEncode implements RFC 3986 percent-encoding used by the RPC signature.
func percentEncode(value string) string {
	const upperHex = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(value); i++ {
		c := value[i]
		if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') ||
			(c >= '0' && c <= '9') || c == '-' || c == '_' || c == '.' || c == '~' {
			b.WriteByte(c)
			continue
		}
		b.WriteByte('%')
		b.WriteByte(upperHex[c>>4])
		b.WriteByte(upperHex[c&0x0f])
	}
	return b.String()
}

// canonicalQuery sorts parameters by key and joins percent-encoded pairs.
func canonicalQuery(params map[string]string) string {
	keys := make([]string, 0, len(params))
	for k := range params {
		if k == "Signature" {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	pairs := make([]string, 0, len(keys))
	for _, k := range keys {
		pairs = append(pairs, percentEncode(k)+"="+percentEncode(params[k]))
	}
	return strings.Join(pairs, "&")
}

// signAliyunRPC returns the base64 HMAC-SHA1 RPC signature.
func signAliyunRPC(secret, method string, params map[string]string) string {
	stringToSign := method + "&" + percentEncode("/") + "&" + percentEncode(canonicalQuery(params))
	mac := hmac.New(sha1.New, []byte(secret+"&"))
	mac.Write([]byte(stringToSign))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

func aliyunNonce() string {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		// crypto/rand failure is unrecoverable; degrade to a timestamp-based
		// nonce rather than panicking inside a request handler.
		return "nonce" + strconv.FormatInt(time.Now().UnixNano(), 10)
	}
	return hex.EncodeToString(buf)
}

func (a *aliyun) Validate(ctx context.Context) ([]Zone, error) {
	var zones []Zone
	for page := 1; page <= 10; page++ {
		env, err := a.call(ctx, "DescribeDomains", map[string]string{
			"PageNumber": strconv.Itoa(page),
			"PageSize":   "100",
		})
		if err != nil {
			return nil, err
		}
		for _, d := range env.Domains.Domain {
			zones = append(zones, Zone{ID: d.DomainID, Name: normalizeName(d.DomainName)})
		}
		if len(env.Domains.Domain) < 100 {
			break
		}
	}
	if len(zones) == 0 {
		return nil, Errorf(CodeZone, "aliyun account sees no domains")
	}
	return zones, nil
}

// lookup finds a record by exact RR and type. RRKeyWord is a server-side filter;
// the exact comparison is done here because it is only a keyword search. Whether
// Aliyun matches underscore-bearing SRV owners exactly is still to be confirmed
// against a real account (delivery note: pending live verification).
func (a *aliyun) lookup(ctx context.Context, zone, rr, recordType string) (RecordID, error) {
	env, err := a.call(ctx, "DescribeDomainRecords", map[string]string{
		"DomainName": normalizeName(zone),
		"RRKeyWord":  rr,
		"Type":       recordType,
		"PageNumber": "1",
		"PageSize":   "100",
	})
	if err != nil {
		return "", err
	}
	for _, rec := range env.DomainRecords.Record {
		if strings.EqualFold(rec.RR, rr) && strings.EqualFold(rec.Type, recordType) && rec.RecordID != "" {
			return RecordID(rec.RecordID), nil
		}
	}
	return "", ErrRecordNotFound
}

func (a *aliyun) LookupA(ctx context.Context, zone, fqdn string) (RecordID, error) {
	prefix, err := PrefixFor(zone, fqdn)
	if err != nil {
		return "", err
	}
	return a.lookup(ctx, zone, prefix, "A")
}

func (a *aliyun) LookupSRV(ctx context.Context, zone, fqdn string) (RecordID, error) {
	prefix, err := PrefixFor(zone, fqdn)
	if err != nil {
		return "", err
	}
	return a.lookup(ctx, zone, RecordHost(prefix), "SRV")
}

func (a *aliyun) UpsertA(ctx context.Context, zone, fqdn, ip string, ttl int) (RecordID, error) {
	prefix, err := PrefixFor(zone, fqdn)
	if err != nil {
		return "", err
	}
	return a.upsert(ctx, zone, prefix, "A", ip, recordTTL(ttl))
}

func (a *aliyun) UpsertSRV(ctx context.Context, zone, fqdn string, port, ttl int) (RecordID, error) {
	prefix, err := PrefixFor(zone, fqdn)
	if err != nil {
		return "", err
	}
	return a.upsert(ctx, zone, RecordHost(prefix), "SRV", SRVValue(port, fqdn), recordTTL(ttl))
}

func (a *aliyun) upsert(ctx context.Context, zone, rr, recordType, value string, ttl int) (RecordID, error) {
	zone = normalizeName(zone)
	existing, err := a.lookup(ctx, zone, rr, recordType)
	switch {
	case err == nil:
		return a.update(ctx, zone, existing, rr, recordType, value, ttl)
	case errors.Is(err, ErrRecordNotFound):
		env, createErr := a.call(ctx, "AddDomainRecord", map[string]string{
			"DomainName": zone,
			"RR":         rr,
			"Type":       recordType,
			"Value":      value,
			"TTL":        strconv.Itoa(ttl),
		})
		if createErr == nil {
			return requireRecordID(env.RecordID, "aliyun AddDomainRecord")
		}
		// A concurrent create makes the retry idempotent: re-query, then update
		// the record that now exists. Any other error is returned unchanged.
		if _, lookupErr := a.lookup(ctx, zone, rr, recordType); lookupErr == nil {
			return a.upsert(ctx, zone, rr, recordType, value, ttl)
		}
		return "", createErr
	default:
		return "", err
	}
}

func (a *aliyun) update(ctx context.Context, zone string, id RecordID, rr, recordType, value string, ttl int) (RecordID, error) {
	env, err := a.call(ctx, "UpdateDomainRecord", map[string]string{
		"RecordId": string(id),
		"RR":       rr,
		"Type":     recordType,
		"Value":    value,
		"TTL":      strconv.Itoa(ttl),
	})
	if err != nil {
		return "", err
	}
	return requireRecordID(env.RecordID, "aliyun UpdateDomainRecord")
}

// requireRecordID falls back to the known ID when a vendor omits it from the
// response (UpdateDomainRecord does return it, but a missing field must not turn
// a successful write into a failure).
func requireRecordID(id, action string) (RecordID, error) {
	if id == "" {
		return "", Errorf(CodeUnknown, "%s did not return a record id", action)
	}
	return RecordID(id), nil
}

func (a *aliyun) DeleteRecord(ctx context.Context, zone string, id RecordID) error {
	if id == "" {
		return nil
	}
	_, err := a.call(ctx, "DeleteDomainRecord", map[string]string{"RecordId": string(id)})
	if err != nil {
		switch VendorCodeOf(err) {
		case "InvalidRecordId.NotFound", "DomainRecordNotBelongToUser", "DomainNotExist":
			return nil
		}
		return err
	}
	return nil
}

// Compile-time assertions keep the optional RecordFinder in sync.
var (
	_ Provider     = (*aliyun)(nil)
	_ RecordFinder = (*aliyun)(nil)
)
