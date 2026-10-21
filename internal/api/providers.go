package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"

	"ts3panel/internal/dns"
	"ts3panel/internal/store"
)

// Provider credentials are never returned in plaintext. The settings page gets
// a first-4/last-4 mask (spec §16.6) and must submit a new value to change it.
const maskedMarker = "****"

var providerIDRE = regexp.MustCompile(`^[a-z0-9-]{1,40}$`)

type providerView struct {
	ID         string            `json:"id"`
	Type       string            `json:"type"`
	Label      string            `json:"label"`
	Zone       string            `json:"zone"`
	ZoneID     string            `json:"zone_id,omitempty"`
	Enabled    bool              `json:"enabled"`
	Default    bool              `json:"default"`
	Creds      map[string]string `json:"creds"`
	Configured bool              `json:"configured"`
}

type providerTestRequest struct {
	ID    string            `json:"id"`
	Type  string            `json:"type"`
	Zone  string            `json:"zone"`
	Creds map[string]string `json:"creds"`
}

type providerTestResponse struct {
	OK        bool       `json:"ok"`
	Zones     []dns.Zone `json:"zones"`
	Zone      string     `json:"zone,omitempty"`
	ZoneFound bool       `json:"zone_found"`
	Message   string     `json:"message"`
}

type providerSaveRequest struct {
	ID      string            `json:"id"`
	Type    string            `json:"type"`
	Label   string            `json:"label"`
	Zone    string            `json:"zone"`
	Creds   map[string]string `json:"creds"`
	Enabled *bool             `json:"enabled"`
	Default *bool             `json:"default"`
}

// providerCredentialKeys lists the documented credential fields per type
// (spec §9.2). Unknown keys are dropped so a caller cannot smuggle arbitrary
// data into the encrypted blob.
var providerCredentialKeys = map[string][]string{
	"cloudflare": {"api_token"},
	"aliyun":     {"access_key_id", "access_key_secret"},
	"dnspod":     {"secret_id", "secret_key"},
	"manual":     nil,
}

func (a *API) handleListProviders(w http.ResponseWriter, r *http.Request) {
	cfg := a.config.Snapshot()
	views := make([]providerView, 0, len(cfg.Providers))
	for _, p := range cfg.Providers {
		views = append(views, a.providerView(p))
	}
	writeJSON(w, http.StatusOK, views)
}

// handleTestProvider validates credentials without saving them. It accepts
// either a saved provider id (credentials are decrypted server-side), raw
// credentials, or a saved provider with an override value.
func (a *API) handleTestProvider(w http.ResponseWriter, r *http.Request) {
	var req providerTestRequest
	if err := decodeJSON(w, r, &req); err != nil {
		a.badRequest(w, "请求格式不正确。", err)
		return
	}

	cfg := a.config.Snapshot()
	provider := store.ProviderConfig{
		Type: strings.ToLower(strings.TrimSpace(req.Type)),
		Zone: store.NormalizeDomain(req.Zone),
	}
	creds := map[string]string{}

	if id := strings.TrimSpace(req.ID); id != "" {
		found := false
		for _, p := range cfg.Providers {
			if p.ID == id {
				provider = p
				found = true
				break
			}
		}
		if !found {
			writeError(w, http.StatusNotFound, "E_NOT_FOUND", "解析平台不存在。", "")
			return
		}
		existing, err := a.providerCredentials(provider)
		if err != nil {
			a.internalError(w, "decrypt provider credentials", err)
			return
		}
		creds = existing
	}
	if strings.TrimSpace(req.Type) != "" {
		provider.Type = strings.ToLower(strings.TrimSpace(req.Type))
	}
	if strings.TrimSpace(req.Zone) != "" {
		provider.Zone = store.NormalizeDomain(req.Zone)
	}
	for key, value := range unmaskedCreds(req.Creds) {
		creds[key] = value
	}

	cleaned, err := normalizedCreds(provider.Type, creds)
	if err != nil {
		writeError(w, http.StatusBadRequest, "E_BAD_REQUEST", err.Error(), "")
		return
	}
	provider.CredsEnc = ""

	p, err := a.buildProvider(provider, cleaned)
	if err != nil {
		writeProviderError(w, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
	defer cancel()
	zones, err := p.Validate(ctx)
	if err != nil {
		writeProviderError(w, err)
		return
	}

	resp := providerTestResponse{OK: true, Zones: zones, Zone: provider.Zone, Message: "凭据有效。"}
	if provider.Zone != "" {
		for _, zone := range zones {
			if strings.EqualFold(zone.Name, provider.Zone) {
				resp.ZoneFound = true
				if provider.ID != "" && zone.ID != "" {
					a.persistZoneID(provider.ID, provider.Zone, zone.ID)
				}
				break
			}
		}
		if !resp.ZoneFound {
			resp.Message = "凭据有效，但当前账号下找不到该主域名，请检查 Zone 填写。"
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

// persistZoneID records the vendor zone ID discovered by a successful test so
// config.toml matches the documented shape (spec §9.2). Failure is non-fatal:
// the panel can always resolve the zone again.
func (a *API) persistZoneID(providerID, zone, zoneID string) {
	err := a.config.Update(func(cfg *store.Config) error {
		for i := range cfg.Providers {
			if cfg.Providers[i].ID == providerID && cfg.Providers[i].Zone == zone && cfg.Providers[i].ZoneID != zoneID {
				cfg.Providers[i].ZoneID = zoneID
			}
		}
		return nil
	})
	if err != nil {
		slog.Warn("persist provider zone id failed", "provider_id", providerID, "err", err)
	}
}

func (a *API) handleCreateProvider(w http.ResponseWriter, r *http.Request) {
	var req providerSaveRequest
	if err := decodeJSON(w, r, &req); err != nil {
		a.badRequest(w, "请求格式不正确。", err)
		return
	}
	typ := strings.ToLower(strings.TrimSpace(req.Type))
	if !isProviderType(typ) {
		writeError(w, http.StatusBadRequest, "E_BAD_REQUEST", "不支持的解析平台。", "")
		return
	}
	zone := store.NormalizeDomain(req.Zone)
	if !store.ValidDomain(zone) {
		writeError(w, http.StatusBadRequest, "E_BAD_REQUEST", "主域名格式不正确。", "例如 example.com。")
		return
	}
	label, err := providerLabel(typ, req.Label)
	if err != nil {
		writeError(w, http.StatusBadRequest, "E_BAD_REQUEST", err.Error(), "")
		return
	}
	creds, err := normalizedCreds(typ, req.Creds)
	if err != nil {
		writeError(w, http.StatusBadRequest, "E_BAD_REQUEST", err.Error(), "")
		return
	}
	credsEnc, err := a.encryptCreds(creds)
	if err != nil {
		a.internalError(w, "encrypt provider credentials", err)
		return
	}

	requestedID := strings.TrimSpace(req.ID)
	if requestedID != "" && !providerIDRE.MatchString(requestedID) {
		writeError(w, http.StatusBadRequest, "E_BAD_REQUEST", "解析平台 ID 只能使用小写字母、数字和连字符。", "")
		return
	}

	var view providerView
	err = a.config.Update(func(cfg *store.Config) error {
		// Idempotent create: the same type+zone is the same provider account.
		for i := range cfg.Providers {
			if cfg.Providers[i].Type == typ && cfg.Providers[i].Zone == zone {
				p := &cfg.Providers[i]
				if requestedID != "" && requestedID != p.ID {
					return fmt.Errorf("provider for %s/%s already exists as %q", typ, zone, p.ID)
				}
				p.Label = label
				p.CredsEnc = credsEnc
				p.Enabled = true
				if req.Enabled != nil {
					p.Enabled = *req.Enabled
				}
				applyProviderDefault(cfg, i, req.Default)
				view = a.providerView(*p)
				return nil
			}
		}
		id := requestedID
		if id == "" {
			id = cfg.NextProviderID(typ)
		}
		for i := range cfg.Providers {
			if cfg.Providers[i].ID == id {
				return fmt.Errorf("provider id %q already exists", id)
			}
		}
		p := store.ProviderConfig{
			ID:       id,
			Type:     typ,
			Label:    label,
			Zone:     zone,
			CredsEnc: credsEnc,
			Enabled:  true,
		}
		cfg.Providers = append(cfg.Providers, p)
		idx := len(cfg.Providers) - 1
		applyProviderDefault(cfg, idx, req.Default)
		if req.Enabled != nil {
			cfg.Providers[idx].Enabled = *req.Enabled
		}
		view = a.providerView(cfg.Providers[idx])
		return nil
	})
	if err != nil {
		writeError(w, http.StatusBadRequest, "E_BAD_REQUEST", err.Error(), "")
		return
	}
	slog.Info("dns provider saved", "provider_id", view.ID, "type", view.Type, "zone", view.Zone)
	writeJSON(w, http.StatusCreated, view)
}

func (a *API) handleUpdateProvider(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	var req providerSaveRequest
	if err := decodeJSON(w, r, &req); err != nil {
		a.badRequest(w, "请求格式不正确。", err)
		return
	}

	var view providerView
	err := a.config.Update(func(cfg *store.Config) error {
		idx := -1
		for i := range cfg.Providers {
			if cfg.Providers[i].ID == id {
				idx = i
				break
			}
		}
		if idx < 0 {
			return errProviderNotFound
		}
		p := &cfg.Providers[idx]

		if typ := strings.ToLower(strings.TrimSpace(req.Type)); typ != "" && typ != p.Type {
			return errors.New("解析平台类型不可修改，请删除后重新添加")
		}
		if zone := store.NormalizeDomain(req.Zone); zone != "" && zone != p.Zone {
			if !store.ValidDomain(zone) {
				return errors.New("主域名格式不正确")
			}
			st, snapErr := a.state.Snapshot()
			if snapErr != nil {
				return snapErr
			}
			for _, inst := range st.Instances {
				if inst.ProviderID == p.ID {
					return errors.New("该解析平台已被实例使用，不能修改主域名")
				}
			}
			p.Zone = zone
		}
		if strings.TrimSpace(req.Label) != "" {
			label, labelErr := providerLabel(p.Type, req.Label)
			if labelErr != nil {
				return labelErr
			}
			p.Label = label
		}
		if incoming := unmaskedCreds(req.Creds); len(incoming) > 0 {
			creds, credErr := normalizedCreds(p.Type, incoming)
			if credErr != nil {
				return credErr
			}
			enc, encErr := a.encryptCreds(creds)
			if encErr != nil {
				return encErr
			}
			p.CredsEnc = enc
		}
		if req.Enabled != nil {
			p.Enabled = *req.Enabled
		}
		applyProviderDefault(cfg, idx, req.Default)
		view = a.providerView(*p)
		return nil
	})
	if err != nil {
		if errors.Is(err, errProviderNotFound) {
			writeError(w, http.StatusNotFound, "E_NOT_FOUND", "解析平台不存在。", "")
			return
		}
		writeError(w, http.StatusBadRequest, "E_BAD_REQUEST", err.Error(), "")
		return
	}
	writeJSON(w, http.StatusOK, view)
}

func (a *API) handleDeleteProvider(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	err := a.config.Update(func(cfg *store.Config) error {
		idx := -1
		for i := range cfg.Providers {
			if cfg.Providers[i].ID == id {
				idx = i
				break
			}
		}
		if idx < 0 {
			return errProviderNotFound
		}
		st, snapErr := a.state.Snapshot()
		if snapErr != nil {
			return snapErr
		}
		for _, inst := range st.Instances {
			if inst.ProviderID == id {
				return fmt.Errorf("实例 %q 正在使用该解析平台", inst.ID)
			}
		}
		wasDefault := cfg.Providers[idx].Default
		cfg.Providers = append(cfg.Providers[:idx], cfg.Providers[idx+1:]...)
		if wasDefault && len(cfg.Providers) > 0 {
			cfg.Providers[0].Default = true
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, errProviderNotFound) {
			writeError(w, http.StatusNotFound, "E_NOT_FOUND", "解析平台不存在。", "")
			return
		}
		writeError(w, http.StatusBadRequest, "E_BAD_REQUEST", err.Error(), "")
		return
	}
	slog.Info("dns provider deleted", "provider_id", id)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// providerView masks every credential value. Configured tells the UI whether a
// secret exists without revealing whether the mask is a placeholder.
func (a *API) providerView(p store.ProviderConfig) providerView {
	view := providerView{
		ID:      p.ID,
		Type:    p.Type,
		Label:   p.Label,
		Zone:    p.Zone,
		ZoneID:  p.ZoneID,
		Enabled: p.Enabled,
		Default: p.Default,
		Creds:   map[string]string{},
	}
	// Manual mode needs no credentials but is fully configured by definition.
	if p.Type == "manual" {
		view.Configured = true
		return view
	}
	creds, err := a.providerCredentials(p)
	if err != nil {
		// A broken ciphertext must not 500 the whole list; report unconfigured
		// and let the operator re-enter credentials.
		slog.Warn("provider credentials unreadable", "provider_id", p.ID, "err", err)
		return view
	}
	for key, value := range creds {
		if strings.TrimSpace(value) == "" {
			continue
		}
		view.Creds[key] = maskCredential(value)
		view.Configured = true
	}
	return view
}

func maskCredential(value string) string {
	runes := []rune(strings.TrimSpace(value))
	if len(runes) == 0 {
		return ""
	}
	if len(runes) <= 8 {
		return maskedMarker
	}
	return string(runes[:4]) + maskedMarker + string(runes[len(runes)-4:])
}

func (a *API) providerCredentials(p store.ProviderConfig) (map[string]string, error) {
	if strings.TrimSpace(p.CredsEnc) == "" {
		return map[string]string{}, nil
	}
	if a.cipher == nil {
		return nil, errors.New("credential cipher is unavailable")
	}
	plain, err := a.cipher.Decrypt(p.CredsEnc)
	if err != nil {
		return nil, err
	}
	creds := map[string]string{}
	if err := json.Unmarshal(plain, &creds); err != nil {
		return nil, errors.New("provider credentials are not valid JSON")
	}
	return creds, nil
}

func (a *API) encryptCreds(creds map[string]string) (string, error) {
	if len(creds) == 0 {
		return "", nil
	}
	if a.cipher == nil {
		return "", errors.New("credential cipher is unavailable")
	}
	plain, err := json.Marshal(creds)
	if err != nil {
		return "", err
	}
	return a.cipher.Encrypt(plain)
}

// buildProvider constructs the vendor client. Tests override providerFactory.
func (a *API) buildProvider(p store.ProviderConfig, creds map[string]string) (dns.Provider, error) {
	if a.providerFactory != nil {
		return a.providerFactory(p, creds)
	}
	return dns.NewProvider(p.Type, creds, p.Zone, p.ZoneID, dns.Options{Client: a.dnsHTTP})
}

// providerForConfig decrypts credentials and builds the provider in one step.
func (a *API) providerForConfig(p store.ProviderConfig) (dns.Provider, error) {
	creds, err := a.providerCredentials(p)
	if err != nil {
		return nil, err
	}
	return a.buildProvider(p, creds)
}

// unmaskedCreds drops empty values and rendered masks so a settings form that
// echoes the current mask back cannot overwrite the stored secret.
func unmaskedCreds(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for key, value := range in {
		value = strings.TrimSpace(value)
		if value == "" || strings.Contains(value, maskedMarker) {
			continue
		}
		out[key] = value
	}
	return out
}

func normalizedCreds(typ string, in map[string]string) (map[string]string, error) {
	keys, ok := providerCredentialKeys[typ]
	if !ok {
		return nil, errors.New("不支持的解析平台。")
	}
	if len(keys) == 0 {
		return map[string]string{}, nil
	}
	out := make(map[string]string, len(keys))
	for _, key := range keys {
		value := strings.TrimSpace(in[key])
		if value == "" || strings.Contains(value, maskedMarker) {
			return nil, fmt.Errorf("请填写完整的 %s 凭据", typ)
		}
		out[key] = value
	}
	return out, nil
}

func isProviderType(typ string) bool {
	_, ok := providerCredentialKeys[typ]
	return ok
}

func providerLabel(typ, label string) (string, error) {
	label = strings.TrimSpace(label)
	if label == "" {
		label = providerDefaultLabels[typ]
	}
	if len([]rune(label)) > 80 {
		return "", errors.New("平台显示名称过长（最多 80 个字符）")
	}
	return label, nil
}

// applyProviderDefault enforces "at most one default". A nil request keeps the
// current value unless no provider is default yet.
func applyProviderDefault(cfg *store.Config, idx int, requested *bool) {
	want := cfg.Providers[idx].Default
	if requested != nil {
		want = *requested
	} else if !want {
		want = true
		for i := range cfg.Providers {
			if i != idx && cfg.Providers[i].Default {
				want = false
				break
			}
		}
	}
	for i := range cfg.Providers {
		if i != idx && want {
			cfg.Providers[i].Default = false
		}
	}
	cfg.Providers[idx].Default = want
}

// writeProviderError maps a DNS error to the documented envelope.
func writeProviderError(w http.ResponseWriter, err error) {
	code := dns.CodeOf(err)
	status, message, hint := dnsErrorText(code)
	if hint == "" {
		hint = truncateText(err.Error(), 300)
	}
	writeError(w, status, code, message, hint)
}

func dnsErrorText(code string) (int, string, string) {
	switch code {
	case dns.CodeAuth:
		return http.StatusBadGateway, "DNS 平台凭据无效或已过期。", "请在设置页重新填写 Token/AccessKey，并先点“测试连接”。"
	case dns.CodePerm:
		return http.StatusBadGateway, "当前凭据没有 DNS 编辑权限。", "Cloudflare 需要 Zone→DNS→Edit；阿里云/腾讯云子账号需要对应的 DNS 权限。"
	case dns.CodeZone:
		return http.StatusBadRequest, "DNS 主域名不存在或不属于当前账号。", "请检查设置中的 Zone 是否与账号下域名完全一致。"
	case dns.CodeRate:
		return http.StatusTooManyRequests, "DNS 平台限流，请稍后再试。", "写入操作是幂等的，稍后重试不会产生重复记录。"
	case dns.CodeConflict:
		return http.StatusConflict, "该子域名已存在不是面板创建的 A 或 SRV 记录。", "确认要覆盖时带上 overwrite=true 重新提交。"
	default:
		return http.StatusBadGateway, "DNS 平台操作失败。", ""
	}
}

func truncateText(value string, max int) string {
	runes := []rune(strings.TrimSpace(value))
	if len(runes) <= max {
		return string(runes)
	}
	return string(runes[:max]) + "…"
}
