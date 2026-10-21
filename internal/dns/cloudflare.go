package dns

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
)

// cloudflare implements Provider against the Cloudflare API v4.
//
// Two details are hard requirements from spec §12.2:
//   - A records are always written with proxied=false (grey cloud);
//   - SRV records must use the structured data object, never a "0 5 ..." string.
type cloudflare struct {
	baseURL string
	token   string
	client  *http.Client

	mu      sync.Mutex
	zoneIDs map[string]string
}

func newCloudflare(baseURL, token, zone, zoneID string, client *http.Client) *cloudflare {
	c := &cloudflare{
		baseURL: strings.TrimRight(baseURL, "/"),
		token:   token,
		client:  client,
		zoneIDs: make(map[string]string, 2),
	}
	if z := normalizeName(zone); z != "" && zoneID != "" {
		c.zoneIDs[z] = zoneID
	}
	return c
}

type cfErrorItem struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type cfEnvelope struct {
	Success    bool            `json:"success"`
	Errors     []cfErrorItem   `json:"errors"`
	Result     json.RawMessage `json:"result"`
	ResultInfo *struct {
		Page       int `json:"page"`
		TotalPages int `json:"total_pages"`
		Count      int `json:"count"`
		TotalCount int `json:"total_count"`
	} `json:"result_info"`
}

// request performs one API call and unwraps the success envelope. A non-2xx
// response with a parseable envelope is reported through its vendor errors.
func (c *cloudflare) request(ctx context.Context, method, path string, body any) (json.RawMessage, error) {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return nil, Errorf(CodeUnknown, "encode cloudflare request: %v", err)
		}
		reader = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return nil, Errorf(CodeUnknown, "build cloudflare request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, Errorf(CodeUnknown, "cloudflare request failed: %s", transportErrorText(err))
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, Errorf(CodeUnknown, "read cloudflare response: %v", err)
	}

	var env cfEnvelope
	if err := json.Unmarshal(data, &env); err != nil {
		return nil, cfHTTPError(resp.StatusCode, "")
	}
	if !env.Success {
		return nil, cfAPIError(resp.StatusCode, env.Errors)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, cfHTTPError(resp.StatusCode, "")
	}
	return env.Result, nil
}

func cfHTTPError(status int, detail string) *Error {
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
	msg := fmt.Sprintf("cloudflare returned HTTP %d", status)
	if detail != "" {
		msg += ": " + detail
	}
	return Errorf(code, "%s", msg)
}

func cfAPIError(status int, items []cfErrorItem) *Error {
	err := &Error{Code: CodeUnknown, Message: "cloudflare rejected the request"}
	if status == http.StatusUnauthorized {
		err.Code = CodeAuth
	}
	if status == http.StatusTooManyRequests {
		err.Code = CodeRate
	}
	if len(items) > 0 {
		first := items[0]
		err.VendorCode = fmt.Sprintf("%d", first.Code)
		err.Message = fmt.Sprintf("cloudflare error %d: %s", first.Code, first.Message)
		switch first.Code {
		case 9109, 10000, 9103, 10001: // invalid token / auth / insufficient credentials
			err.Code = CodeAuth
		case 971, 1015: // rate limited
			err.Code = CodeRate
		case 7003, 7000: // zone not found / no route to zone
			err.Code = CodeZone
		}
	}
	if status == http.StatusForbidden && err.Code == CodeUnknown {
		err.Code = CodePerm
	}
	return err
}

func (c *cloudflare) cachedZoneID(zone string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.zoneIDs[zone]
}

func (c *cloudflare) cacheZoneID(zone, id string) {
	if zone == "" || id == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.zoneIDs[zone] = id
}

// zoneID resolves the zone name to its Cloudflare zone ID, querying at most
// once per zone per process.
func (c *cloudflare) zoneID(ctx context.Context, zone string) (string, error) {
	z := normalizeName(zone)
	if z == "" {
		return "", Errorf(CodeZone, "cloudflare zone is empty")
	}
	if id := c.cachedZoneID(z); id != "" {
		return id, nil
	}
	raw, err := c.request(ctx, http.MethodGet, "/zones?name="+url.QueryEscape(z)+"&per_page=50", nil)
	if err != nil {
		if errors.Is(err, ErrRecordNotFound) || CodeOf(err) == CodeZone {
			return "", Errorf(CodeZone, "cloudflare zone %q not found", z)
		}
		return "", err
	}
	var zones []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &zones); err != nil {
		return "", Errorf(CodeUnknown, "decode cloudflare zones: %v", err)
	}
	for _, item := range zones {
		if normalizeName(item.Name) == z && item.ID != "" {
			c.cacheZoneID(z, item.ID)
			return item.ID, nil
		}
	}
	return "", Errorf(CodeZone, "cloudflare zone %q not found", z)
}

// Validate lists the zones the token can read. It follows pagination so a busy
// account is not silently truncated.
func (c *cloudflare) Validate(ctx context.Context) ([]Zone, error) {
	var out []Zone
	for page := 1; page <= 10; page++ {
		raw, err := c.request(ctx, http.MethodGet,
			fmt.Sprintf("/zones?per_page=50&page=%d", page), nil)
		if err != nil {
			return nil, err
		}
		var zones []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		}
		if err := json.Unmarshal(raw, &zones); err != nil {
			return nil, Errorf(CodeUnknown, "decode cloudflare zones: %v", err)
		}
		for _, item := range zones {
			name := normalizeName(item.Name)
			out = append(out, Zone{ID: item.ID, Name: name})
			c.cacheZoneID(name, item.ID)
		}
		if len(zones) < 50 {
			break
		}
	}
	if len(out) == 0 {
		return nil, Errorf(CodeZone, "cloudflare token sees no zones")
	}
	return out, nil
}

type cfDNSRecord struct {
	ID   string `json:"id"`
	Type string `json:"type"`
	Name string `json:"name"`
}

// findRecord returns the ID of an existing record of type at the exact name.
func (c *cloudflare) findRecord(ctx context.Context, zone, recordType, name string) (RecordID, error) {
	zid, err := c.zoneID(ctx, zone)
	if err != nil {
		return "", err
	}
	query := url.Values{}
	query.Set("type", recordType)
	query.Set("name", name)
	query.Set("per_page", "10")
	raw, err := c.request(ctx, http.MethodGet,
		"/zones/"+url.PathEscape(zid)+"/dns_records?"+query.Encode(), nil)
	if err != nil {
		return "", err
	}
	var records []cfDNSRecord
	if err := json.Unmarshal(raw, &records); err != nil {
		return "", Errorf(CodeUnknown, "decode cloudflare dns records: %v", err)
	}
	want := normalizeName(name)
	for _, rec := range records {
		if rec.ID != "" && strings.EqualFold(rec.Type, recordType) && normalizeName(rec.Name) == want {
			return RecordID(rec.ID), nil
		}
	}
	return "", ErrRecordNotFound
}

func (c *cloudflare) LookupA(ctx context.Context, zone, fqdn string) (RecordID, error) {
	return c.findRecord(ctx, zone, "A", normalizeName(fqdn))
}

func (c *cloudflare) LookupSRV(ctx context.Context, zone, fqdn string) (RecordID, error) {
	prefix, err := PrefixFor(zone, fqdn)
	if err != nil {
		return "", err
	}
	return c.findRecord(ctx, zone, "SRV", RecordHost(prefix)+"."+normalizeName(zone))
}

// UpsertA writes the A record with proxied=false. Cloudflare reports 81057 when
// the name already exists and 9003 for a TTL it refuses; both are recovered by
// re-querying and by falling back to TTL=1 (automatic), as required in §12.2.
func (c *cloudflare) UpsertA(ctx context.Context, zone, fqdn, ip string, ttl int) (RecordID, error) {
	return c.upsertA(ctx, normalizeName(fqdn), zone, ip, recordTTL(ttl), true)
}

func (c *cloudflare) upsertA(ctx context.Context, fqdn, zone, ip string, ttl int, allowTTLFallback bool) (RecordID, error) {
	id, err := c.LookupA(ctx, zone, fqdn)
	switch {
	case err == nil:
		rid, saveErr := c.saveA(ctx, zone, id, fqdn, ip, ttl, http.MethodPut)
		if saveErr == nil {
			return rid, nil
		}
		if allowTTLFallback && VendorCodeOf(saveErr) == "9003" {
			return c.upsertA(ctx, fqdn, zone, ip, 1, false)
		}
		return "", saveErr
	case errors.Is(err, ErrRecordNotFound):
		rid, saveErr := c.saveA(ctx, zone, "", fqdn, ip, ttl, http.MethodPost)
		if saveErr == nil {
			return rid, nil
		}
		switch VendorCodeOf(saveErr) {
		case "81057":
			// The record appeared after the lookup; take it over by ID.
			if _, lookupErr := c.LookupA(ctx, zone, fqdn); lookupErr == nil {
				return c.upsertA(ctx, fqdn, zone, ip, ttl, allowTTLFallback)
			}
		case "9003":
			if allowTTLFallback {
				return c.upsertA(ctx, fqdn, zone, ip, 1, false)
			}
		}
		return "", saveErr
	default:
		return "", err
	}
}

func (c *cloudflare) saveA(ctx context.Context, zone string, id RecordID, fqdn, ip string, ttl int, method string) (RecordID, error) {
	zid, err := c.zoneID(ctx, zone)
	if err != nil {
		return "", err
	}
	body := struct {
		Type    string `json:"type"`
		Name    string `json:"name"`
		Content string `json:"content"`
		Proxied bool   `json:"proxied"`
		TTL     int    `json:"ttl"`
		Comment string `json:"comment"`
	}{
		Type:    "A",
		Name:    fqdn,
		Content: ip,
		Proxied: false,
		TTL:     ttl,
		Comment: "ts3panel:" + strings.SplitN(fqdn, ".", 2)[0],
	}
	path := "/zones/" + url.PathEscape(zid) + "/dns_records"
	if method == http.MethodPut {
		path += "/" + url.PathEscape(string(id))
	}
	raw, err := c.request(ctx, method, path, body)
	if err != nil {
		return "", err
	}
	return cfResultID(raw)
}

// UpsertSRV writes _ts3._udp.<fqdn> with a data object (spec §12.2).
func (c *cloudflare) UpsertSRV(ctx context.Context, zone, fqdn string, port, ttl int) (RecordID, error) {
	prefix, err := PrefixFor(zone, fqdn)
	if err != nil {
		return "", err
	}
	return c.upsertSRV(ctx, zone, RecordHost(prefix)+"."+normalizeName(zone), prefix, fqdn, port, recordTTL(ttl), true)
}

func (c *cloudflare) upsertSRV(ctx context.Context, zone, name, prefix, fqdn string, port, ttl int, allowTTLFallback bool) (RecordID, error) {
	id, err := c.findRecord(ctx, zone, "SRV", name)
	switch {
	case err == nil:
		rid, saveErr := c.saveSRV(ctx, zone, id, name, prefix, fqdn, port, ttl, http.MethodPut)
		if saveErr == nil {
			return rid, nil
		}
		if allowTTLFallback && VendorCodeOf(saveErr) == "9003" {
			return c.upsertSRV(ctx, zone, name, prefix, fqdn, port, 1, false)
		}
		return "", saveErr
	case errors.Is(err, ErrRecordNotFound):
		rid, saveErr := c.saveSRV(ctx, zone, "", name, prefix, fqdn, port, ttl, http.MethodPost)
		if saveErr == nil {
			return rid, nil
		}
		switch VendorCodeOf(saveErr) {
		case "81057":
			if _, lookupErr := c.findRecord(ctx, zone, "SRV", name); lookupErr == nil {
				return c.upsertSRV(ctx, zone, name, prefix, fqdn, port, ttl, allowTTLFallback)
			}
		case "9003":
			if allowTTLFallback {
				return c.upsertSRV(ctx, zone, name, prefix, fqdn, port, 1, false)
			}
		}
		return "", saveErr
	default:
		return "", err
	}
}

type cfSRVData struct {
	Service  string `json:"service"`
	Proto    string `json:"proto"`
	Name     string `json:"name"`
	Priority int    `json:"priority"`
	Weight   int    `json:"weight"`
	Port     int    `json:"port"`
	Target   string `json:"target"`
}

func (c *cloudflare) saveSRV(ctx context.Context, zone string, id RecordID, name, prefix, fqdn string, port, ttl int, method string) (RecordID, error) {
	zid, err := c.zoneID(ctx, zone)
	if err != nil {
		return "", err
	}
	body := struct {
		Type    string    `json:"type"`
		Name    string    `json:"name"`
		TTL     int       `json:"ttl"`
		Comment string    `json:"comment"`
		Data    cfSRVData `json:"data"`
	}{
		Type:    "SRV",
		Name:    name,
		TTL:     ttl,
		Comment: "ts3panel:" + prefix,
		Data: cfSRVData{
			Service:  "_ts3",
			Proto:    "_udp",
			Name:     prefix,
			Priority: 0,
			Weight:   5,
			Port:     port,
			Target:   normalizeName(fqdn),
		},
	}
	path := "/zones/" + url.PathEscape(zid) + "/dns_records"
	if method == http.MethodPut {
		path += "/" + url.PathEscape(string(id))
	}
	raw, err := c.request(ctx, method, path, body)
	if err != nil {
		return "", err
	}
	return cfResultID(raw)
}

func cfResultID(raw json.RawMessage) (RecordID, error) {
	var result struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return "", Errorf(CodeUnknown, "decode cloudflare record result: %v", err)
	}
	if result.ID == "" {
		return "", Errorf(CodeUnknown, "cloudflare did not return a record id")
	}
	return RecordID(result.ID), nil
}

// DeleteRecord deletes one record by ID. 81044 (record not found) is treated as
// success so repeated deletes are idempotent.
func (c *cloudflare) DeleteRecord(ctx context.Context, zone string, id RecordID) error {
	if id == "" {
		return nil
	}
	zid, err := c.zoneID(ctx, zone)
	if err != nil {
		return err
	}
	path := "/zones/" + url.PathEscape(zid) + "/dns_records/" + url.PathEscape(string(id))
	if _, err := c.request(ctx, http.MethodDelete, path, nil); err != nil {
		if VendorCodeOf(err) == "81044" || CodeOf(err) == CodeZone {
			return nil
		}
		return err
	}
	return nil
}

// recordTTL applies the documented default of 300 seconds.
func recordTTL(ttl int) int {
	if ttl <= 0 {
		return 300
	}
	return ttl
}
