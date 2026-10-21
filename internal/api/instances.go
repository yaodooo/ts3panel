package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"ts3panel/internal/orchestrator"
	"ts3panel/internal/store"
)

var errProviderNotFound = errors.New("provider not found")

type createInstanceRequest struct {
	Name       string `json:"name"`
	Subdomain  string `json:"subdomain"`
	ProviderID string `json:"provider_id"`
}

// writeInstanceError maps orchestrator errors onto the documented E_* codes.
// Error details are placed in "hint" so the message stays stable and Chinese.
func writeInstanceError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, orchestrator.ErrInstanceNotFound):
		writeError(w, http.StatusNotFound, "E_INSTANCE_NOT_FOUND", "实例不存在。", "")
	case errors.Is(err, orchestrator.ErrInstanceExists), errors.Is(err, orchestrator.ErrSubdomainTaken):
		writeError(w, http.StatusConflict, "E_SUBDOMAIN_TAKEN", "该子域名已被其他实例占用。", err.Error())
	case errors.Is(err, orchestrator.ErrSubdomainInvalid):
		writeError(w, http.StatusBadRequest, "E_SUBDOMAIN_INVALID", "子域名格式不正确。", "仅允许小写字母、数字和连字符，长度 1–20。")
	case errors.Is(err, orchestrator.ErrNameInvalid):
		writeError(w, http.StatusBadRequest, "E_BAD_REQUEST", "实例名称不正确。", "名称长度 1–40，不能包含控制字符。")
	case errors.Is(err, orchestrator.ErrLimitReached):
		writeError(w, http.StatusConflict, "E_LIMIT_REACHED", "已达到实例数量上限。", "可在 config.toml 的 limits.max_instances 中调整。")
	case errors.Is(err, orchestrator.ErrPortExhausted):
		writeError(w, http.StatusConflict, "E_PORT_EXHAUSTED", "连续 50 个端口组都被占用，无法分配端口。", "请检查宿主机端口占用情况。")
	case errors.Is(err, orchestrator.ErrContainerTimeout):
		writeError(w, http.StatusInternalServerError, "E_CONTAINER_TIMEOUT", "容器未能在 60 秒内进入运行状态。", err.Error())
	case errors.Is(err, orchestrator.ErrNotManaged):
		writeError(w, http.StatusConflict, "E_DOCKER", "同名容器不属于 ts3panel，拒绝操作。", err.Error())
	case errors.Is(err, context.DeadlineExceeded):
		writeError(w, http.StatusGatewayTimeout, "E_DOCKER", "操作超时。", "Docker 或镜像拉取耗时过长，请稍后重试。")
	case errors.Is(err, orchestrator.ErrDocker):
		writeError(w, http.StatusInternalServerError, "E_DOCKER", "Docker 操作失败。", err.Error())
	default:
		slog.Error("instance operation failed", "err", err)
		writeError(w, http.StatusInternalServerError, "E_INTERNAL", "服务器内部错误。", "")
	}
}

func (a *API) handleCreateInstance(w http.ResponseWriter, r *http.Request) {
	var req createInstanceRequest
	if err := decodeJSON(w, r, &req); err != nil {
		a.badRequest(w, "请求格式不正确。", err)
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		writeError(w, http.StatusBadRequest, "E_BAD_REQUEST", "请填写实例名称。", "")
		return
	}

	cfg := a.config.Snapshot()
	id, fqdn, providerID, err := resolveInstanceAddress(cfg, req.Subdomain, strings.TrimSpace(req.ProviderID))
	if err != nil {
		if errors.Is(err, errProviderNotFound) {
			writeError(w, http.StatusBadRequest, "E_BAD_REQUEST", "解析平台不存在。", err.Error())
			return
		}
		writeError(w, http.StatusBadRequest, "E_SUBDOMAIN_INVALID", "子域名格式不正确。", err.Error())
		return
	}

	// Pulling an image plus waiting for TS3 can exceed the server's default
	// write deadline; extend it for this request only.
	if rc := http.NewResponseController(w); rc != nil {
		_ = rc.SetWriteDeadline(time.Now().Add(6 * time.Minute))
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()

	view, err := a.instances.Create(ctx, orchestrator.CreateParams{
		ID:         id,
		Name:       name,
		Subdomain:  fqdn,
		ProviderID: providerID,
	})
	if err != nil {
		writeInstanceError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, view)
}

func (a *API) handleListInstances(w http.ResponseWriter, r *http.Request) {
	views, err := a.instances.List(r.Context())
	if err != nil {
		a.internalError(w, "list instances", err)
		return
	}
	writeJSON(w, http.StatusOK, views)
}

// providerBrief describes the instance's DNS provider without exposing any
// credential material. The detail page uses it to render the manual-mode warning
// (errata E9) and the zone the records belong to.
type providerBrief struct {
	ID    string `json:"id"`
	Type  string `json:"type"`
	Label string `json:"label"`
	Zone  string `json:"zone"`
}

// instanceDetailResponse adds panel-side metadata to the orchestrator detail.
type instanceDetailResponse struct {
	orchestrator.InstanceDetail
	DNSManual bool           `json:"dns_manual"`
	Provider  *providerBrief `json:"provider,omitempty"`
	PublicIP  string         `json:"public_ip,omitempty"`
}

func (a *API) handleGetInstance(w http.ResponseWriter, r *http.Request) {
	// A detail request may need two Docker stats samples plus a directory walk;
	// cap it well below the server-wide write timeout.
	ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
	defer cancel()

	detail, err := a.instances.Detail(ctx, r.PathValue("id"))
	if err != nil {
		writeInstanceError(w, err)
		return
	}
	resp := instanceDetailResponse{InstanceDetail: detail}
	cfg := a.config.Snapshot()
	for i := range cfg.Providers {
		p := cfg.Providers[i]
		if p.ID != detail.ProviderID {
			continue
		}
		resp.Provider = &providerBrief{ID: p.ID, Type: p.Type, Label: p.Label, Zone: p.Zone}
		// Manual mode records are owned by the operator, not the panel (E9).
		resp.DNSManual = p.Type == "manual"
		break
	}
	resp.PublicIP = cfg.Cloudflare.PublicIP
	writeJSON(w, http.StatusOK, resp)
}

func (a *API) handleStartInstance(w http.ResponseWriter, r *http.Request) {
	a.instanceLifecycle(w, r, a.instances.Start)
}

func (a *API) handleStopInstance(w http.ResponseWriter, r *http.Request) {
	a.instanceLifecycle(w, r, a.instances.Stop)
}

func (a *API) handleRestartInstance(w http.ResponseWriter, r *http.Request) {
	a.instanceLifecycle(w, r, a.instances.Restart)
}

func (a *API) instanceLifecycle(w http.ResponseWriter, r *http.Request, op func(context.Context, string) (orchestrator.InstanceView, error)) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	view, err := op(ctx, r.PathValue("id"))
	if err != nil {
		writeInstanceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

func (a *API) handleDeleteInstance(w http.ResponseWriter, r *http.Request) {
	purge := parseBoolQuery(r, "purge")
	dnsFlag := parseBoolQuery(r, "dns")

	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()

	// Delete the panel-owned DNS records first, by stored record ID. A DNS
	// failure aborts the delete so the instance and its records stay
	// consistent; the request is idempotent and can simply be retried.
	if dnsFlag {
		lock := a.dnsLock(r.PathValue("id"))
		lock.Lock()
		inst, err := a.instanceByID(r.PathValue("id"))
		if err == nil {
			err = a.deleteInstanceRecords(ctx, inst)
		}
		lock.Unlock()
		if err != nil {
			a.writeDomainError(w, err)
			return
		}
	}

	if err := a.instances.Delete(ctx, r.PathValue("id"), purge); err != nil {
		writeInstanceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "purged": purge, "dns_removed": dnsFlag})
}

func parseBoolQuery(r *http.Request, key string) bool {
	value := strings.ToLower(strings.TrimSpace(r.URL.Query().Get(key)))
	return value == "1" || value == "true" || value == "yes"
}

// resolveInstanceAddress validates the subdomain field and derives the immutable
// instance id. Input may be a prefix ("ts1") or a full name ("ts1.example.com");
// when a provider with a zone is selected, a prefix is expanded to an FQDN.
func resolveInstanceAddress(cfg store.Config, raw, providerID string) (id, fqdn, provID string, err error) {
	raw = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(raw)), ".")
	if raw == "" {
		return "", "", "", fmt.Errorf("%w: 子域名不能为空", orchestrator.ErrSubdomainInvalid)
	}

	var provider *store.ProviderConfig
	if providerID != "" {
		for i := range cfg.Providers {
			if cfg.Providers[i].ID == providerID {
				provider = &cfg.Providers[i]
				break
			}
		}
		if provider == nil {
			return "", "", "", fmt.Errorf("%w: %q", errProviderNotFound, providerID)
		}
	}

	if strings.Contains(raw, ".") {
		if !store.ValidDomain(raw) {
			return "", "", "", fmt.Errorf("%w: %q 不是合法域名", orchestrator.ErrSubdomainInvalid, raw)
		}
		prefix := strings.SplitN(raw, ".", 2)[0]
		if err := validateSlug(prefix); err != nil {
			return "", "", "", fmt.Errorf("%w: %s", orchestrator.ErrSubdomainInvalid, err.Error())
		}
		if provider != nil && provider.Zone != "" {
			zone := provider.Zone
			if raw != zone && !strings.HasSuffix(raw, "."+zone) {
				return "", "", "", fmt.Errorf("%w: 子域名必须位于 %s 下", orchestrator.ErrSubdomainInvalid, zone)
			}
		}
		return prefix, raw, providerID, nil
	}

	if err := validateSlug(raw); err != nil {
		return "", "", "", fmt.Errorf("%w: %s", orchestrator.ErrSubdomainInvalid, err.Error())
	}
	fqdn = raw
	if provider != nil && provider.Zone != "" {
		fqdn = raw + "." + provider.Zone
	}
	return raw, fqdn, providerID, nil
}

// parseMemoryLimit converts values such as "512m" or "1g" into bytes. Invalid
// or empty input yields 0 so the manager's documented default applies.
func parseMemoryLimit(value string) int64 {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		return 0
	}
	multiplier := int64(1)
	switch {
	case strings.HasSuffix(value, "g"):
		multiplier = 1024 * 1024 * 1024
		value = strings.TrimSuffix(value, "g")
	case strings.HasSuffix(value, "m"):
		multiplier = 1024 * 1024
		value = strings.TrimSuffix(value, "m")
	case strings.HasSuffix(value, "k"):
		multiplier = 1024
		value = strings.TrimSuffix(value, "k")
	}
	n, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
	if err != nil || n <= 0 {
		return 0
	}
	return n * multiplier
}

// parseCPULimit converts "1.0" into Docker's NanoCPUs unit.
func parseCPULimit(value string) int64 {
	f, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
	if err != nil || f <= 0 {
		return 0
	}
	return int64(f * 1_000_000_000)
}
