package dns

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	dnspodAPIVersion = "2021-03-23"
	dnspodService    = "dnspod"
	dnspodSigning    = "tc3_request"
	dnspodContent    = "application/json; charset=utf-8"
)

// dnspod implements Provider against the DNSPod API3 (Tencent Cloud TC3
// signature). No Tencent SDK is used (spec §12.2).
type dnspod struct {
	endpoint  string
	host      string
	secretID  string
	secretKey string
	client    *http.Client
	now       func() time.Time
}

func newDNSPod(endpoint, secretID, secretKey string, client *http.Client, now func() time.Time) *dnspod {
	host := "dnspod.tencentcloudapi.com"
	if parsed, err := url.Parse(endpoint); err == nil && parsed.Host != "" {
		host = parsed.Host
	}
	return &dnspod{
		endpoint:  endpoint,
		host:      host,
		secretID:  secretID,
		secretKey: secretKey,
		client:    client,
		now:       now,
	}
}

type dnspodError struct {
	Code    string `json:"Code"`
	Message string `json:"Message"`
}

type dnspodEnvelope struct {
	Response struct {
		RequestID  string       `json:"RequestId"`
		Error      *dnspodError `json:"Error"`
		DomainList []struct {
			ID   json.Number `json:"ID"`
			Name string      `json:"Name"`
		} `json:"DomainList"`
		RecordList []struct {
			RecordID json.Number `json:"RecordId"`
			Name     string      `json:"Name"`
			Type     string      `json:"Type"`
			Value    string      `json:"Value"`
		} `json:"RecordList"`
		RecordID json.Number `json:"RecordId"`
	} `json:"Response"`
}

func (d *dnspod) call(ctx context.Context, action string, payload any) (json.RawMessage, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, Errorf(CodeUnknown, "encode dnspod request: %v", err)
	}
	timestamp := d.now().Unix()
	authorization := dnspodAuthorization(d.secretID, d.secretKey, d.host, action, body, timestamp)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, Errorf(CodeUnknown, "build dnspod request: %v", err)
	}
	req.Host = d.host
	req.Header.Set("Content-Type", dnspodContent)
	req.Header.Set("X-TC-Action", action)
	req.Header.Set("X-TC-Timestamp", strconv.FormatInt(timestamp, 10))
	req.Header.Set("X-TC-Version", dnspodAPIVersion)
	req.Header.Set("Authorization", authorization)
	req.Header.Set("Accept", "application/json")

	resp, err := d.client.Do(req)
	if err != nil {
		return nil, Errorf(CodeUnknown, "dnspod request failed: %s", transportErrorText(err))
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, Errorf(CodeUnknown, "read dnspod response: %v", err)
	}

	var env dnspodEnvelope
	if err := json.Unmarshal(data, &env); err != nil {
		return nil, dnspodHTTPError(resp.StatusCode, "")
	}
	if env.Response.Error != nil && env.Response.Error.Code != "" {
		return nil, dnspodAPIError(resp.StatusCode, env.Response.Error.Code, env.Response.Error.Message)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, dnspodHTTPError(resp.StatusCode, "")
	}
	return json.RawMessage(data), nil
}

func dnspodDecode(raw json.RawMessage) (dnspodEnvelope, error) {
	var env dnspodEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return dnspodEnvelope{}, Errorf(CodeUnknown, "decode dnspod result: %v", err)
	}
	return env, nil
}

func dnspodHTTPError(status int, detail string) *Error {
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
	msg := fmt.Sprintf("dnspod returned HTTP %d", status)
	if detail != "" {
		msg += ": " + detail
	}
	return Errorf(code, "%s", msg)
}

func dnspodAPIError(status int, vendorCode, message string) *Error {
	err := &Error{Code: CodeUnknown, VendorCode: vendorCode,
		Message: fmt.Sprintf("dnspod error %s: %s", vendorCode, message)}
	switch {
	case strings.HasPrefix(vendorCode, "AuthFailure"):
		err.Code = CodeAuth
	case strings.HasPrefix(vendorCode, "UnauthorizedOperation"),
		strings.HasPrefix(vendorCode, "OperationDenied"):
		err.Code = CodePerm
	case vendorCode == "ResourceNotFound.NoDataOfDomain",
		vendorCode == "ResourceNotFound.DomainNotExist",
		strings.HasPrefix(vendorCode, "InvalidParameter.Domain"):
		err.Code = CodeZone
	case strings.HasPrefix(vendorCode, "RequestLimitExceeded"),
		strings.HasPrefix(vendorCode, "LimitExceeded"):
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

// sha256Hex is the lowercase hex SHA-256 used throughout the TC3 algorithm.
func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func hmacSHA256(key, data []byte) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write(data)
	return mac.Sum(nil)
}

// dnspodAuthorization builds the TC3-HMAC-SHA256 Authorization header:
//
//	TC3-HMAC-SHA256 Credential=<id>/<date>/dnspod/tc3_request,
//	  SignedHeaders=content-type;host;x-tc-action, Signature=<hex>
func dnspodAuthorization(secretID, secretKey, host, action string, payload []byte, timestamp int64) string {
	date := time.Unix(timestamp, 0).UTC().Format("2006-01-02")
	canonicalHeaders := "content-type:" + dnspodContent + "\n" +
		"host:" + host + "\n" +
		"x-tc-action:" + strings.ToLower(action) + "\n"
	signedHeaders := "content-type;host;x-tc-action"
	canonicalRequest := http.MethodPost + "\n" +
		"/\n" +
		"\n" + // empty canonical query string
		canonicalHeaders + "\n" +
		signedHeaders + "\n" +
		sha256Hex(payload)

	scope := date + "/" + dnspodService + "/" + dnspodSigning
	stringToSign := "TC3-HMAC-SHA256\n" +
		strconv.FormatInt(timestamp, 10) + "\n" +
		scope + "\n" +
		sha256Hex([]byte(canonicalRequest))

	secretDate := hmacSHA256([]byte("TC3"+secretKey), []byte(date))
	secretService := hmacSHA256(secretDate, []byte(dnspodService))
	secretSigning := hmacSHA256(secretService, []byte(dnspodSigning))
	signature := hex.EncodeToString(hmacSHA256(secretSigning, []byte(stringToSign)))

	return "TC3-HMAC-SHA256 Credential=" + secretID + "/" + scope +
		", SignedHeaders=" + signedHeaders + ", Signature=" + signature
}

func (d *dnspod) Validate(ctx context.Context) ([]Zone, error) {
	var zones []Zone
	for offset := 0; offset < 1000; offset += 100 {
		raw, err := d.call(ctx, "DescribeDomainList", map[string]any{
			"Offset": offset,
			"Limit":  100,
		})
		if err != nil {
			return nil, err
		}
		env, err := dnspodDecode(raw)
		if err != nil {
			return nil, err
		}
		for _, item := range env.Response.DomainList {
			name := normalizeName(item.Name)
			if name == "" {
				continue
			}
			zones = append(zones, Zone{ID: item.ID.String(), Name: name})
		}
		if len(env.Response.DomainList) < 100 {
			break
		}
	}
	if len(zones) == 0 {
		return nil, Errorf(CodeZone, "dnspod account sees no domains")
	}
	return zones, nil
}

func (d *dnspod) lookup(ctx context.Context, zone, rr, recordType string) (RecordID, error) {
	raw, err := d.call(ctx, "DescribeRecordList", map[string]any{
		"Domain":     normalizeName(zone),
		"Subdomain":  rr,
		"RecordType": recordType,
		"Offset":     0,
		"Limit":      100,
	})
	if err != nil {
		return "", err
	}
	env, err := dnspodDecode(raw)
	if err != nil {
		return "", err
	}
	for _, rec := range env.Response.RecordList {
		if strings.EqualFold(rec.Name, rr) && strings.EqualFold(rec.Type, recordType) && rec.RecordID.String() != "" {
			return RecordID(rec.RecordID.String()), nil
		}
	}
	return "", ErrRecordNotFound
}

func (d *dnspod) LookupA(ctx context.Context, zone, fqdn string) (RecordID, error) {
	prefix, err := PrefixFor(zone, fqdn)
	if err != nil {
		return "", err
	}
	return d.lookup(ctx, zone, prefix, "A")
}

func (d *dnspod) LookupSRV(ctx context.Context, zone, fqdn string) (RecordID, error) {
	prefix, err := PrefixFor(zone, fqdn)
	if err != nil {
		return "", err
	}
	return d.lookup(ctx, zone, RecordHost(prefix), "SRV")
}

func (d *dnspod) UpsertA(ctx context.Context, zone, fqdn, ip string, ttl int) (RecordID, error) {
	prefix, err := PrefixFor(zone, fqdn)
	if err != nil {
		return "", err
	}
	return d.upsert(ctx, zone, prefix, "A", ip, recordTTL(ttl))
}

func (d *dnspod) UpsertSRV(ctx context.Context, zone, fqdn string, port, ttl int) (RecordID, error) {
	prefix, err := PrefixFor(zone, fqdn)
	if err != nil {
		return "", err
	}
	return d.upsert(ctx, zone, RecordHost(prefix), "SRV", SRVValue(port, fqdn), recordTTL(ttl))
}

// upsert uses RecordLine "默认"; DNSPod requires the line on both create and
// modify. The SRV SubDomain form and the "0 5 <port> <target>" RDATA are pinned
// by unit tests; both must still be confirmed against a real DNSPod account
// (delivery note: pending live verification).
func (d *dnspod) upsert(ctx context.Context, zone, rr, recordType, value string, ttl int) (RecordID, error) {
	zone = normalizeName(zone)
	existing, err := d.lookup(ctx, zone, rr, recordType)
	switch {
	case err == nil:
		recordID, parseErr := strconv.ParseUint(string(existing), 10, 64)
		if parseErr != nil {
			return "", Errorf(CodeUnknown, "dnspod returned a non-numeric record id")
		}
		raw, modifyErr := d.call(ctx, "ModifyRecord", map[string]any{
			"Domain":     zone,
			"RecordId":   recordID,
			"SubDomain":  rr,
			"RecordType": recordType,
			"RecordLine": "默认",
			"Value":      value,
			"TTL":        ttl,
		})
		if modifyErr != nil {
			return "", modifyErr
		}
		return dnspodRecordID(raw, existing)
	case errors.Is(err, ErrRecordNotFound):
		raw, createErr := d.call(ctx, "CreateRecord", map[string]any{
			"Domain":     zone,
			"SubDomain":  rr,
			"RecordType": recordType,
			"RecordLine": "默认",
			"Value":      value,
			"TTL":        ttl,
		})
		if createErr == nil {
			return dnspodRecordID(raw, "")
		}
		// A concurrent create makes the retry idempotent: re-query, then update.
		if _, lookupErr := d.lookup(ctx, zone, rr, recordType); lookupErr == nil {
			return d.upsert(ctx, zone, rr, recordType, value, ttl)
		}
		return "", createErr
	default:
		return "", err
	}
}

func dnspodRecordID(raw json.RawMessage, fallback RecordID) (RecordID, error) {
	env, err := dnspodDecode(raw)
	if err != nil {
		return "", err
	}
	if id := env.Response.RecordID.String(); id != "" && id != "0" {
		return RecordID(id), nil
	}
	if fallback != "" {
		return fallback, nil
	}
	return "", Errorf(CodeUnknown, "dnspod did not return a record id")
}

func (d *dnspod) DeleteRecord(ctx context.Context, zone string, id RecordID) error {
	if id == "" {
		return nil
	}
	recordID, err := strconv.ParseUint(string(id), 10, 64)
	if err != nil {
		return nil // a non-numeric ID cannot exist at DNSPod; treat as gone
	}
	_, err = d.call(ctx, "DeleteRecord", map[string]any{
		"Domain":   normalizeName(zone),
		"RecordId": recordID,
	})
	if err != nil {
		switch VendorCodeOf(err) {
		case "ResourceNotFound.NoDataOfRecord", "InvalidParameter.RecordIdInvalid":
			return nil
		}
		return err
	}
	return nil
}

var (
	_ Provider     = (*dnspod)(nil)
	_ RecordFinder = (*dnspod)(nil)
)
