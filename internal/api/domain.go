package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"ts3panel/internal/dns"
	"ts3panel/internal/model"
	"ts3panel/internal/orchestrator"
	"ts3panel/internal/store"
)

// The DNS layer never rolls back or deletes an instance (spec F-17 hard
// constraint). A failed bind leaves the running server intact and returns the
// manual checklist plus retryable=true so the operator can recover without
// touching the container.

var (
	errInstanceGone    = errors.New("instance no longer exists")
	errPublicIPMissing = errors.New("public ip is not configured and could not be detected")
)

type domainBindRequest struct {
	Subdomain  string `json:"subdomain"`
	ProviderID string `json:"provider_id"`
	// Overwrite allows replacing an A/SRV record the panel does not own (F-14).
	Overwrite bool `json:"overwrite"`
	// Verify controls whether this request also waits for DNS propagation. The
	// detail/create wizard sends false so it can render "写解析" and "验证" as
	// two distinct progress steps, then calls POST .../domain/verify. Omitted
	// means true, preserving the documented single-call behaviour.
	Verify *bool `json:"verify"`
}

// domainPrecheckResponse answers the create-wizard conflict check (F-14) before
// an instance exists. A vendor lookup failure is reported as Checked=false with
// a warning instead of blocking creation: the authoritative failure will still
// surface, with a manual checklist, when the bind itself runs.
type domainPrecheckResponse struct {
	OK            bool     `json:"ok"`
	FQDN          string   `json:"fqdn"`
	Zone          string   `json:"zone,omitempty"`
	ProviderID    string   `json:"provider_id,omitempty"`
	Available     bool     `json:"available"`
	LocalConflict bool     `json:"local_conflict"`
	Conflicts     []string `json:"conflicts,omitempty"`
	Checked       bool     `json:"checked"`
	Warning       string   `json:"warning,omitempty"`
	Message       string   `json:"message"`
}

type domainBindResponse struct {
	OK          bool               `json:"ok"`
	InstanceID  string             `json:"instance_id"`
	ProviderID  string             `json:"provider_id"`
	Subdomain   string             `json:"subdomain"`
	Zone        string             `json:"zone"`
	DNSStatus   string             `json:"dns_status"`
	ARecordID   string             `json:"a_record_id,omitempty"`
	SRVRecordID string             `json:"srv_record_id,omitempty"`
	Verified    bool               `json:"verified"`
	Retryable   bool               `json:"retryable"`
	Message     string             `json:"message"`
	Manual      []dns.ManualRecord `json:"manual,omitempty"`
	ManualText  string             `json:"manual_text,omitempty"`
	Verify      *dns.CheckResult   `json:"verify,omitempty"`
}

type domainVerifyResponse struct {
	OK         bool               `json:"ok"`
	InstanceID string             `json:"instance_id"`
	Subdomain  string             `json:"subdomain"`
	DNSStatus  string             `json:"dns_status"`
	Verified   bool               `json:"verified"`
	Retryable  bool               `json:"retryable"`
	Message    string             `json:"message"`
	Verify     *dns.CheckResult   `json:"verify,omitempty"`
	Manual     []dns.ManualRecord `json:"manual,omitempty"`
	ManualText string             `json:"manual_text,omitempty"`
}

type manualDomainResponse struct {
	OK         bool               `json:"ok"`
	InstanceID string             `json:"instance_id"`
	Zone       string             `json:"zone"`
	Subdomain  string             `json:"subdomain"`
	PublicIP   string             `json:"public_ip"`
	Records    []dns.ManualRecord `json:"records"`
	Text       string             `json:"text"`
	DNSStatus  string             `json:"dns_status"`
}

// domainFailureResponse keeps the documented error envelope and adds the manual
// checklist and retry marker required by spec §11.3 / §15.4.
type domainFailureResponse struct {
	Error      apiErrorBody       `json:"error"`
	InstanceID string             `json:"instance_id,omitempty"`
	DNSStatus  string             `json:"dns_status"`
	Retryable  bool               `json:"retryable"`
	Missing    []string           `json:"missing,omitempty"`
	Manual     []dns.ManualRecord `json:"manual,omitempty"`
	ManualText string             `json:"manual_text,omitempty"`
}

// dnsLock serializes DNS mutations for one instance. Instances are capped at 20,
// so keeping one mutex per instance id is bounded.
func (a *API) dnsLock(id string) *sync.Mutex {
	value, _ := a.dnsLocks.LoadOrStore(id, &sync.Mutex{})
	return value.(*sync.Mutex)
}

func (a *API) instanceByID(id string) (model.Instance, error) {
	st, err := a.state.Snapshot()
	if err != nil {
		return model.Instance{}, err
	}
	for i := range st.Instances {
		if st.Instances[i].ID == id {
			return st.Instances[i], nil
		}
	}
	return model.Instance{}, fmt.Errorf("%w: %q", orchestrator.ErrInstanceNotFound, id)
}

func (a *API) patchInstance(id string, fn func(*model.Instance)) error {
	return a.state.Update(func(st *store.State) error {
		for i := range st.Instances {
			if st.Instances[i].ID == id {
				fn(&st.Instances[i])
				return nil
			}
		}
		return fmt.Errorf("%w: %q", errInstanceGone, id)
	})
}

func (a *API) dnsTimeout() time.Duration {
	timeout := dns.VerifyTimeout
	if a.verifier != nil && a.verifier.Timeout > 0 {
		timeout = a.verifier.Timeout
	}
	return timeout + 30*time.Second
}

// extendWriteDeadline lets a bind/verify request outlive the server's 60s
// WriteTimeout while the verifier polls for up to 180 seconds.
func extendWriteDeadline(w http.ResponseWriter, extra time.Duration) {
	if rc := http.NewResponseController(w); rc != nil {
		_ = rc.SetWriteDeadline(time.Now().Add(extra))
	}
}

// selectProvider picks the requested provider, the instance's previous one, or
// the configured default, in that order.
func (a *API) selectProvider(cfg store.Config, inst model.Instance, requested string) (store.ProviderConfig, error) {
	id := strings.TrimSpace(requested)
	if id == "" {
		id = strings.TrimSpace(inst.ProviderID)
	}
	if id == "" {
		for i := range cfg.Providers {
			if cfg.Providers[i].Default {
				id = cfg.Providers[i].ID
				break
			}
		}
	}
	if id == "" && len(cfg.Providers) > 0 {
		id = cfg.Providers[0].ID
	}
	if id == "" {
		return store.ProviderConfig{}, fmt.Errorf("%w: no provider configured", errProviderNotFound)
	}
	for i := range cfg.Providers {
		if cfg.Providers[i].ID == id {
			if !cfg.Providers[i].Enabled {
				return store.ProviderConfig{}, fmt.Errorf("解析平台 %q 已被禁用", id)
			}
			return cfg.Providers[i], nil
		}
	}
	return store.ProviderConfig{}, fmt.Errorf("%w: %q", errProviderNotFound, id)
}

// resolveBindingTarget expands a prefix or validates a full FQDN inside the
// provider zone and enforces the single-label instance prefix rule.
func resolveBindingTarget(zone, raw, current string) (fqdn, prefix string, err error) {
	zone = store.NormalizeDomain(zone)
	raw = store.NormalizeDomain(raw)
	if raw == "" {
		raw = store.NormalizeDomain(current)
	}
	if raw == "" {
		return "", "", fmt.Errorf("%w: 子域名不能为空", orchestrator.ErrSubdomainInvalid)
	}
	if !strings.Contains(raw, ".") {
		if err := validateSlug(raw); err != nil {
			return "", "", fmt.Errorf("%w: %s", orchestrator.ErrSubdomainInvalid, err.Error())
		}
		raw = raw + "." + zone
	}
	if !store.ValidDomain(raw) {
		return "", "", fmt.Errorf("%w: %q 不是合法域名", orchestrator.ErrSubdomainInvalid, raw)
	}
	prefix, err = dns.PrefixFor(zone, raw)
	if err != nil {
		return "", "", fmt.Errorf("%w: 子域名必须位于 %s 下", orchestrator.ErrSubdomainInvalid, zone)
	}
	if err := validateSlug(prefix); err != nil {
		return "", "", fmt.Errorf("%w: 子域名前缀需为 1–20 位小写字母、数字或连字符", orchestrator.ErrSubdomainInvalid)
	}
	return raw, prefix, nil
}

func (a *API) ensureSubdomainFree(id, fqdn string) error {
	st, err := a.state.Snapshot()
	if err != nil {
		return err
	}
	for i := range st.Instances {
		if st.Instances[i].ID == id {
			continue
		}
		if strings.EqualFold(st.Instances[i].Subdomain, fqdn) {
			return fmt.Errorf("%w: %q", orchestrator.ErrSubdomainTaken, fqdn)
		}
	}
	return nil
}

// publicIP returns the configured public IP, falling back to a one-time probe
// as described in spec §12.5. The value is cached for the process lifetime.
func (a *API) publicIP(ctx context.Context) (string, error) {
	cfg := a.config.Snapshot()
	if ip := strings.TrimSpace(cfg.Cloudflare.PublicIP); ip != "" {
		return ip, nil
	}

	a.publicIPMu.Lock()
	defer a.publicIPMu.Unlock()
	if a.publicIPCached != "" {
		return a.publicIPCached, nil
	}

	probeCtx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	var ip string
	var err error
	if a.publicIPDetector != nil {
		ip, err = a.publicIPDetector(probeCtx)
	} else {
		ip, err = dns.DetectPublicIP(probeCtx, a.dnsHTTP)
	}
	if err != nil {
		slog.Warn("public ip detection failed", "err", err)
		return "", errPublicIPMissing
	}
	ip = strings.TrimSpace(ip)
	if len(ip) == 0 {
		return "", errPublicIPMissing
	}
	a.publicIPCached = ip
	return ip, nil
}

func manualRecordsFor(zone, fqdn, publicIP string, port, ttl int) []dns.ManualRecord {
	records, err := dns.ManualRecords(zone, fqdn, publicIP, port, ttl)
	if err != nil {
		return nil
	}
	return records
}

func manualText(zone string, records []dns.ManualRecord) string {
	var b strings.Builder
	if zone != "" {
		fmt.Fprintf(&b, "请在 %s 的 DNS 控制台添加：\n\n", zone)
	} else {
		b.WriteString("请在 DNS 控制台添加：\n\n")
	}
	b.WriteString("类型\t主机记录\t记录值\n")
	for _, record := range records {
		fmt.Fprintf(&b, "%s\t%s\t%s\n", record.Type, record.Host, record.Value)
	}
	b.WriteString("\nTTL 使用 300 秒。")
	return b.String()
}

// handleDomainPrecheck performs the F-14 conflict check before an instance is
// created. It answers three questions: is the prefix already used by another
// instance, does a provider record with that name already exist, and is the
// resolution platform even usable right now. Only a hard local conflict makes
// Available=false; vendor/credential problems are reported as warnings so the
// operator can still create the instance and bind later.
func (a *API) handleDomainPrecheck(w http.ResponseWriter, r *http.Request) {
	raw := r.URL.Query().Get("subdomain")
	providerID := strings.TrimSpace(r.URL.Query().Get("provider_id"))
	// Rebinding an existing instance must not conflict with itself.
	excludeID := strings.TrimSpace(r.URL.Query().Get("instance_id"))
	cfg := a.config.Snapshot()

	id, fqdn, provID, err := resolveInstanceAddress(cfg, raw, providerID)
	if err != nil {
		if errors.Is(err, errProviderNotFound) {
			writeError(w, http.StatusBadRequest, "E_BAD_REQUEST", "解析平台不存在。", "")
			return
		}
		a.writeDomainError(w, err)
		return
	}

	resp := domainPrecheckResponse{OK: true, FQDN: fqdn, ProviderID: provID, Available: true}
	var providerCfg *store.ProviderConfig
	for i := range cfg.Providers {
		if cfg.Providers[i].ID == provID {
			providerCfg = &cfg.Providers[i]
			break
		}
	}
	if providerCfg != nil {
		resp.Zone = providerCfg.Zone
	}

	st, err := a.state.Snapshot()
	if err != nil {
		a.internalError(w, "precheck state", err)
		return
	}
	var self *model.Instance
	for i := range st.Instances {
		inst := &st.Instances[i]
		if excludeID != "" && inst.ID == excludeID {
			self = inst
			continue
		}
		if inst.ID == id || strings.EqualFold(inst.Subdomain, fqdn) {
			resp.Available = false
			resp.LocalConflict = true
			resp.Message = fmt.Sprintf("实例「%s」已使用该子域名或前缀，请换一个。", inst.Name)
			writeJSON(w, http.StatusOK, resp)
			return
		}
	}

	if providerCfg == nil {
		resp.Message = "未选择解析平台：实例会先按 IP:端口 可用，之后可在详情页绑定域名。"
		writeJSON(w, http.StatusOK, resp)
		return
	}
	if providerCfg.Type == "manual" {
		resp.Message = "手动解析模式：面板不会检查 DNS 控制台中已有的记录，请自行确认后添加。"
		writeJSON(w, http.StatusOK, resp)
		return
	}

	provider, buildErr := a.providerForConfig(*providerCfg)
	if buildErr != nil {
		resp.Warning = "解析平台凭据不可用，已跳过现有记录检查。"
		resp.Message = "不影响创建实例；绑定失败时会给出可复制的解析清单。"
		writeJSON(w, http.StatusOK, resp)
		return
	}
	finder, ok := provider.(dns.RecordFinder)
	if !ok {
		resp.Warning = "该解析平台不支持按名称预检记录，已跳过冲突检查。"
		resp.Message = "不影响创建实例。"
		writeJSON(w, http.StatusOK, resp)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 12*time.Second)
	defer cancel()
	var conflicts []string
	if recordID, lookupErr := finder.LookupA(ctx, providerCfg.Zone, fqdn); lookupErr == nil {
		// A record this instance already owns is not a conflict when rebinding.
		if self == nil || self.DNS.ARecordID == "" || string(recordID) != self.DNS.ARecordID {
			conflicts = append(conflicts, "A")
		}
	} else if !errors.Is(lookupErr, dns.ErrRecordNotFound) {
		resp.Warning = "查询现有 A 记录失败，已跳过冲突检查。"
		resp.Message = "不影响创建实例；绑定时会再次检查并给出明确错误。"
		writeJSON(w, http.StatusOK, resp)
		return
	}
	if recordID, lookupErr := finder.LookupSRV(ctx, providerCfg.Zone, fqdn); lookupErr == nil {
		if self == nil || self.DNS.SRVRecordID == "" || string(recordID) != self.DNS.SRVRecordID {
			conflicts = append(conflicts, "SRV")
		}
	} else if !errors.Is(lookupErr, dns.ErrRecordNotFound) {
		resp.Warning = "查询现有 SRV 记录失败，已跳过冲突检查。"
		resp.Message = "不影响创建实例；绑定时会再次检查并给出明确错误。"
		writeJSON(w, http.StatusOK, resp)
		return
	}

	resp.Checked = true
	if len(conflicts) > 0 {
		resp.Available = false
		resp.Conflicts = conflicts
		resp.Message = "该子域名已有 " + strings.Join(conflicts, "、") + " 记录；继续绑定会覆盖这些记录。"
	} else {
		resp.Message = "未发现冲突，可以绑定。"
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleBindDomain writes A + SRV, then verifies with real DNS lookups. Any
// failure keeps the instance untouched and returns the checklist.
func (a *API) handleBindDomain(w http.ResponseWriter, r *http.Request) {
	var req domainBindRequest
	if err := decodeJSON(w, r, &req); err != nil {
		a.badRequest(w, "请求格式不正确。", err)
		return
	}
	id := r.PathValue("id")
	lock := a.dnsLock(id)
	lock.Lock()
	defer lock.Unlock()

	timeout := a.dnsTimeout()
	extendWriteDeadline(w, timeout+10*time.Second)
	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()

	cfg := a.config.Snapshot()
	inst, err := a.instanceByID(id)
	if err != nil {
		a.writeDomainError(w, err)
		return
	}
	providerCfg, err := a.selectProvider(cfg, inst, req.ProviderID)
	if err != nil {
		a.writeDomainError(w, err)
		return
	}
	fqdn, _, err := resolveBindingTarget(providerCfg.Zone, req.Subdomain, inst.Subdomain)
	if err != nil {
		a.writeDomainError(w, err)
		return
	}
	if err := a.ensureSubdomainFree(inst.ID, fqdn); err != nil {
		a.writeDomainError(w, err)
		return
	}
	ip, err := a.publicIP(ctx)
	if err != nil {
		writeError(w, http.StatusBadRequest, "E_BAD_REQUEST",
			"缺少服务器公网 IP，无法写入 A 记录。", "请在设置中填写公网 IP（NAT 云主机必须手填）。")
		return
	}
	provider, err := a.providerForConfig(providerCfg)
	if err != nil {
		var dnsErr *dns.Error
		if errors.As(err, &dnsErr) {
			status, message, hint := dnsErrorText(dns.CodeOf(err))
			a.writeDNSFailure(w, status, dns.CodeOf(err), message, hint,
				inst, providerCfg.Zone, fqdn, ip, []string{"A", "SRV"}, true, inst.DNSStatus)
			return
		}
		a.writeDomainError(w, err)
		return
	}
	ttl := cfg.Limits.RecordTTL
	if ttl <= 0 {
		ttl = 300
	}

	// Manual mode: no API calls, only a checklist. Old panel-owned records (if
	// any) are removed first so a switch to manual does not leave stale entries.
	if dns.IsManual(provider) {
		if err := a.teardownOldRecords(ctx, inst, fqdn, providerCfg.ID); err != nil {
			a.writeDomainError(w, err)
			return
		}
		if err := a.patchInstance(inst.ID, func(target *model.Instance) {
			target.Subdomain = fqdn
			target.ProviderID = providerCfg.ID
			target.DNSStatus = model.DNSStatusNone
			target.DNS = model.DNSInfo{}
		}); err != nil {
			a.writeDomainError(w, err)
			return
		}
		records := manualRecordsFor(providerCfg.Zone, fqdn, ip, inst.Ports.Voice, ttl)
		writeJSON(w, http.StatusOK, domainBindResponse{
			OK:         true,
			InstanceID: inst.ID,
			ProviderID: providerCfg.ID,
			Subdomain:  fqdn,
			Zone:       providerCfg.Zone,
			DNSStatus:  model.DNSStatusNone,
			Retryable:  false,
			Message:    "手动解析模式：请按清单在 DNS 控制台添加记录，然后点“我已添加，去验证”。",
			Manual:     records,
			ManualText: manualText(providerCfg.Zone, records),
		})
		return
	}

	sameTarget := strings.EqualFold(inst.Subdomain, fqdn) && inst.ProviderID == providerCfg.ID

	// F-14: refuse to overwrite a record the panel does not own unless the
	// caller explicitly confirms. The check runs before any write and before
	// old records are removed.
	if !req.Overwrite {
		if conflicts, conflictErr := a.detectConflict(ctx, provider, providerCfg.Zone, fqdn, inst, sameTarget); conflictErr != nil {
			var dnsErr *dns.Error
			if errors.As(conflictErr, &dnsErr) {
				status, message, hint := dnsErrorText(dns.CodeOf(conflictErr))
				a.writeDNSFailure(w, status, dns.CodeOf(conflictErr), message, hint,
					inst, providerCfg.Zone, fqdn, ip, []string{"A", "SRV"}, true, inst.DNSStatus)
				return
			}
			a.writeDomainError(w, conflictErr)
			return
		} else if len(conflicts) > 0 {
			a.writeDNSFailure(w, http.StatusConflict, dns.CodeConflict,
				"该子域名已存在不是面板创建的 DNS 记录。",
				"确认要覆盖已有记录时，请带上 overwrite=true 重新提交。",
				inst, providerCfg.Zone, fqdn, ip, conflicts, true, model.DNSStatusNone)
			return
		}
	}

	if !sameTarget {
		if err := a.teardownOldRecords(ctx, inst, fqdn, providerCfg.ID); err != nil {
			a.writeDomainError(w, err)
			return
		}
	}

	// Persist the pending intent before touching DNS so an interrupted bind is
	// visible and retryable. Clearing IDs when the target changes avoids
	// deleting the new records later by stale IDs.
	if err := a.patchInstance(inst.ID, func(target *model.Instance) {
		target.Subdomain = fqdn
		target.ProviderID = providerCfg.ID
		target.DNSStatus = model.DNSStatusPending
		if !sameTarget {
			target.DNS = model.DNSInfo{}
		}
	}); err != nil {
		a.writeDomainError(w, err)
		return
	}

	aID, err := provider.UpsertA(ctx, providerCfg.Zone, fqdn, ip, ttl)
	if err != nil {
		// Keep any record IDs from a previous successful bind: nothing was
		// deleted on this path, so they still describe records the panel owns.
		// A changed target already had its IDs cleared by the pending patch.
		_ = a.patchInstance(inst.ID, func(target *model.Instance) {
			target.DNSStatus = model.DNSStatusFailed
		})
		status, message, hint := dnsErrorText(dns.CodeOf(err))
		a.writeDNSFailure(w, status, dns.CodeOf(err), message, hint,
			inst, providerCfg.Zone, fqdn, ip, []string{"A", "SRV"}, true, model.DNSStatusFailed)
		return
	}
	if err := a.patchInstance(inst.ID, func(target *model.Instance) {
		target.DNS.ARecordID = string(aID)
	}); err != nil {
		a.writeDomainError(w, err)
		return
	}

	srvID, err := provider.UpsertSRV(ctx, providerCfg.Zone, fqdn, inst.Ports.Voice, ttl)
	if err != nil {
		_ = a.patchInstance(inst.ID, func(target *model.Instance) {
			target.DNSStatus = model.DNSStatusFailed
			target.DNS.ARecordID = string(aID)
		})
		status, message, hint := dnsErrorText(dns.CodeOf(err))
		a.writeDNSFailure(w, status, dns.CodeOf(err), message, hint,
			inst, providerCfg.Zone, fqdn, ip, []string{"SRV"}, true, model.DNSStatusFailed)
		return
	}
	if err := a.patchInstance(inst.ID, func(target *model.Instance) {
		target.DNS.SRVRecordID = string(srvID)
	}); err != nil {
		a.writeDomainError(w, err)
		return
	}

	// The wizard splits writing and verifying into two requests so each step can
	// show real progress. The state stays "pending" until /domain/verify runs.
	if req.Verify != nil && !*req.Verify {
		writeJSON(w, http.StatusOK, domainBindResponse{
			OK:          true,
			InstanceID:  inst.ID,
			ProviderID:  providerCfg.ID,
			Subdomain:   fqdn,
			Zone:        providerCfg.Zone,
			DNSStatus:   model.DNSStatusPending,
			ARecordID:   string(aID),
			SRVRecordID: string(srvID),
			Verified:    false,
			Retryable:   true,
			Message:     "A 与 SRV 记录已写入，正在验证解析生效。",
		})
		return
	}

	result := a.verifyTarget(ctx, fqdn, inst.Ports.Voice, ip)
	status := model.DNSStatusFailed
	var verifiedAt *time.Time
	if result.Verified {
		status = model.DNSStatusVerified
		now := result.CheckedAt
		verifiedAt = &now
	}
	if err := a.patchInstance(inst.ID, func(target *model.Instance) {
		target.DNSStatus = status
		target.DNS.VerifiedAt = verifiedAt
	}); err != nil {
		a.writeDomainError(w, err)
		return
	}

	resp := domainBindResponse{
		OK:          true,
		InstanceID:  inst.ID,
		ProviderID:  providerCfg.ID,
		Subdomain:   fqdn,
		Zone:        providerCfg.Zone,
		DNSStatus:   status,
		ARecordID:   string(aID),
		SRVRecordID: string(srvID),
		Verified:    result.Verified,
		Retryable:   !result.Verified,
		Message:     result.Message,
		Verify:      &result,
	}
	if !result.Verified {
		records := manualRecordsFor(providerCfg.Zone, fqdn, ip, inst.Ports.Voice, ttl)
		resp.Manual = records
		resp.ManualText = manualText(providerCfg.Zone, records)
	}
	slog.Info("dns bind completed", "instance_id", inst.ID, "provider_id", providerCfg.ID,
		"fqdn", fqdn, "dns_status", status, "verified", result.Verified)
	writeJSON(w, http.StatusOK, resp)
}

// detectConflict looks up the target records and reports which ones exist but
// were not created by this binding.
func (a *API) detectConflict(ctx context.Context, provider dns.Provider, zone, fqdn string, inst model.Instance, sameTarget bool) ([]string, error) {
	finder, ok := provider.(dns.RecordFinder)
	if !ok {
		return nil, nil
	}
	var conflicts []string
	aID, err := finder.LookupA(ctx, zone, fqdn)
	switch {
	case err == nil:
		owned := sameTarget && inst.DNS.ARecordID != "" && string(aID) == inst.DNS.ARecordID
		if !owned {
			conflicts = append(conflicts, "A")
		}
	case errors.Is(err, dns.ErrRecordNotFound):
	default:
		return nil, err
	}
	srvID, err := finder.LookupSRV(ctx, zone, fqdn)
	switch {
	case err == nil:
		owned := sameTarget && inst.DNS.SRVRecordID != "" && string(srvID) == inst.DNS.SRVRecordID
		if !owned {
			conflicts = append(conflicts, "SRV")
		}
	case errors.Is(err, dns.ErrRecordNotFound):
	default:
		return nil, err
	}
	return conflicts, nil
}

// teardownOldRecords deletes panel-owned records when the subdomain or provider
// changes. Deleting by stored ID (never by name) is a hard requirement (§12.3).
func (a *API) teardownOldRecords(ctx context.Context, inst model.Instance, newFQDN, newProviderID string) error {
	if strings.EqualFold(inst.Subdomain, newFQDN) && inst.ProviderID == newProviderID {
		return nil
	}
	return a.deleteInstanceRecords(ctx, inst)
}

func (a *API) deleteInstanceRecords(ctx context.Context, inst model.Instance) error {
	if inst.DNS.ARecordID == "" && inst.DNS.SRVRecordID == "" {
		return nil
	}
	cfg := a.config.Snapshot()
	var providerCfg *store.ProviderConfig
	for i := range cfg.Providers {
		if cfg.Providers[i].ID == inst.ProviderID {
			providerCfg = &cfg.Providers[i]
			break
		}
	}
	if providerCfg == nil {
		return fmt.Errorf("%w: DNS provider %q for instance %q is missing; delete the records manually",
			errProviderNotFound, inst.ProviderID, inst.ID)
	}
	provider, err := a.providerForConfig(*providerCfg)
	if err != nil {
		return err
	}
	// SRV first: spec §12.3.3.
	if inst.DNS.SRVRecordID != "" {
		if err := provider.DeleteRecord(ctx, providerCfg.Zone, dns.RecordID(inst.DNS.SRVRecordID)); err != nil {
			return err
		}
	}
	if inst.DNS.ARecordID != "" {
		if err := provider.DeleteRecord(ctx, providerCfg.Zone, dns.RecordID(inst.DNS.ARecordID)); err != nil {
			return err
		}
	}
	return nil
}

func (a *API) verifyTarget(ctx context.Context, fqdn string, port int, ip string) dns.CheckResult {
	verifier := a.verifier
	if verifier == nil {
		verifier = dns.NewVerifier()
	}
	return verifier.Wait(ctx, fqdn, port, ip)
}

func (a *API) handleVerifyDomain(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	lock := a.dnsLock(id)
	lock.Lock()
	defer lock.Unlock()

	timeout := a.dnsTimeout()
	extendWriteDeadline(w, timeout+10*time.Second)
	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()

	inst, err := a.instanceByID(id)
	if err != nil {
		a.writeDomainError(w, err)
		return
	}
	if !strings.Contains(inst.Subdomain, ".") {
		writeError(w, http.StatusBadRequest, "E_SUBDOMAIN_INVALID",
			"该实例尚未绑定完整子域名，无法验证。", "请先绑定域名或使用手动解析清单。")
		return
	}
	ip, err := a.publicIP(ctx)
	if err != nil {
		writeError(w, http.StatusBadRequest, "E_BAD_REQUEST",
			"缺少服务器公网 IP，无法验证 A 记录。", "请在设置中填写公网 IP。")
		return
	}

	result := a.verifyTarget(ctx, inst.Subdomain, inst.Ports.Voice, ip)
	status := model.DNSStatusFailed
	var verifiedAt *time.Time
	if result.Verified {
		status = model.DNSStatusVerified
		now := result.CheckedAt
		verifiedAt = &now
	}
	if err := a.patchInstance(inst.ID, func(target *model.Instance) {
		target.DNSStatus = status
		target.DNS.VerifiedAt = verifiedAt
	}); err != nil {
		a.writeDomainError(w, err)
		return
	}

	cfg := a.config.Snapshot()
	zone := ""
	ttl := cfg.Limits.RecordTTL
	if ttl <= 0 {
		ttl = 300
	}
	if providerCfg, selErr := a.selectProvider(cfg, inst, ""); selErr == nil {
		zone = providerCfg.Zone
	}
	resp := domainVerifyResponse{
		OK:         true,
		InstanceID: inst.ID,
		Subdomain:  inst.Subdomain,
		DNSStatus:  status,
		Verified:   result.Verified,
		Retryable:  !result.Verified,
		Message:    result.Message,
		Verify:     &result,
	}
	if !result.Verified && zone != "" {
		records := manualRecordsFor(zone, inst.Subdomain, ip, inst.Ports.Voice, ttl)
		resp.Manual = records
		resp.ManualText = manualText(zone, records)
	}
	writeJSON(w, http.StatusOK, resp)
}

func (a *API) handleUnbindDomain(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	lock := a.dnsLock(id)
	lock.Lock()
	defer lock.Unlock()

	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()

	inst, err := a.instanceByID(id)
	if err != nil {
		a.writeDomainError(w, err)
		return
	}
	if err := a.deleteInstanceRecords(ctx, inst); err != nil {
		a.writeDomainError(w, err)
		return
	}
	if err := a.patchInstance(inst.ID, func(target *model.Instance) {
		target.DNSStatus = model.DNSStatusNone
		target.DNS = model.DNSInfo{}
	}); err != nil {
		a.writeDomainError(w, err)
		return
	}
	slog.Info("dns records unbound", "instance_id", inst.ID)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":          true,
		"instance_id": inst.ID,
		"dns_status":  model.DNSStatusNone,
	})
}

func (a *API) handleManualDomain(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	inst, err := a.instanceByID(id)
	if err != nil {
		a.writeDomainError(w, err)
		return
	}
	cfg := a.config.Snapshot()
	providerCfg, err := a.selectProvider(cfg, inst, "")
	if err != nil {
		a.writeDomainError(w, err)
		return
	}
	fqdn := inst.Subdomain
	if !strings.Contains(fqdn, ".") {
		fqdn = store.NormalizeDomain(fqdn + "." + providerCfg.Zone)
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	ip, err := a.publicIP(ctx)
	if err != nil {
		writeError(w, http.StatusBadRequest, "E_BAD_REQUEST",
			"缺少服务器公网 IP，无法生成 A 记录清单。", "请在设置中填写公网 IP。")
		return
	}
	ttl := cfg.Limits.RecordTTL
	if ttl <= 0 {
		ttl = 300
	}
	records := manualRecordsFor(providerCfg.Zone, fqdn, ip, inst.Ports.Voice, ttl)
	if records == nil {
		writeError(w, http.StatusBadRequest, "E_SUBDOMAIN_INVALID",
			"实例子域名不在所选主域名之下，无法生成清单。", "")
		return
	}
	writeJSON(w, http.StatusOK, manualDomainResponse{
		OK:         true,
		InstanceID: inst.ID,
		Zone:       providerCfg.Zone,
		Subdomain:  fqdn,
		PublicIP:   ip,
		Records:    records,
		Text:       manualText(providerCfg.Zone, records),
		DNSStatus:  inst.DNSStatus,
	})
}

// writeDNSFailure emits a DNS error plus the manual checklist. Used for write
// failures and conflicts; the instance itself is never modified here.
func (a *API) writeDNSFailure(w http.ResponseWriter, status int, code, message, hint string,
	inst model.Instance, zone, fqdn, ip string, missing []string, retryable bool, dnsStatus string) {
	ttl := a.config.Snapshot().Limits.RecordTTL
	if ttl <= 0 {
		ttl = 300
	}
	records := manualRecordsFor(zone, fqdn, ip, inst.Ports.Voice, ttl)
	writeJSON(w, status, domainFailureResponse{
		Error:      apiErrorBody{Code: code, Message: message, Hint: hint},
		InstanceID: inst.ID,
		DNSStatus:  dnsStatus,
		Retryable:  retryable,
		Missing:    missing,
		Manual:     records,
		ManualText: manualText(zone, records),
	})
}

func (a *API) writeDomainError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, orchestrator.ErrInstanceNotFound), errors.Is(err, errInstanceGone):
		writeError(w, http.StatusNotFound, "E_INSTANCE_NOT_FOUND", "实例不存在。", "")
	case errors.Is(err, errProviderNotFound):
		writeError(w, http.StatusBadRequest, "E_BAD_REQUEST", "解析平台不存在或已被删除。", "请在设置页重新添加解析平台。")
	case errors.Is(err, dns.ErrZoneMismatch):
		writeError(w, http.StatusBadRequest, "E_SUBDOMAIN_INVALID", "子域名与解析平台的主域名不匹配。", err.Error())
	case errors.Is(err, orchestrator.ErrSubdomainInvalid):
		writeError(w, http.StatusBadRequest, "E_SUBDOMAIN_INVALID", "子域名格式不正确。", err.Error())
	case errors.Is(err, orchestrator.ErrSubdomainTaken):
		writeError(w, http.StatusConflict, "E_SUBDOMAIN_TAKEN", "该子域名已被其他实例占用。", "")
	case errors.Is(err, errPublicIPMissing):
		writeError(w, http.StatusBadRequest, "E_BAD_REQUEST", "无法确定服务器公网 IP。", "请在设置中填写公网 IP。")
	default:
		var dnsErr *dns.Error
		if errors.As(err, &dnsErr) {
			writeProviderError(w, err)
			return
		}
		slog.Error("domain operation failed", "err", err)
		writeError(w, http.StatusInternalServerError, "E_INTERNAL", "服务器内部错误。", "")
	}
}
