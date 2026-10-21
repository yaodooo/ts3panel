package api

import (
	"context"
	"net"
	"net/http"
	"strings"
	"time"

	"ts3panel/internal/dns"
	"ts3panel/internal/store"
)

// settingsView is the subset of config.toml the settings page may read. Secrets
// are deliberately absent: provider credentials have their own endpoint and are
// always masked there.
type settingsView struct {
	PublicIP      string `json:"public_ip"`
	ExternalCheck bool   `json:"external_check"`
	DetectedIP    string `json:"detected_ip,omitempty"`
	DetectError   string `json:"detect_error,omitempty"`
}

type settingsUpdateRequest struct {
	// PublicIP is a pointer so an omitted field cannot silently wipe a configured
	// address; an explicit empty string clears it.
	PublicIP *string `json:"public_ip"`
	// ExternalCheck toggles the optional third-party port probe (check-host.net).
	ExternalCheck *bool `json:"external_check"`
}

// handleGetSettings returns the configured public IP. With ?detect=1 it also
// attempts the documented probe. A failed probe returns HTTP 200 with
// detect_error: per errata E8 a NAT host that cannot reach the probe service is
// normal and must not block the page or the operator.
func (a *API) handleGetSettings(w http.ResponseWriter, r *http.Request) {
	cfg := a.config.Snapshot()
	view := settingsView{
		PublicIP:      strings.TrimSpace(cfg.Cloudflare.PublicIP),
		ExternalCheck: cfg.Ports.ExternalCheck,
	}
	if parseBoolQuery(r, "detect") {
		detected, detectErr := a.detectPublicIP(r.Context())
		view.DetectedIP = detected
		view.DetectError = detectErr
	}
	writeJSON(w, http.StatusOK, view)
}

// handleUpdateSettings saves the manually entered public IP. Manual input has
// priority over detection (errata E8); an empty value simply clears the override
// so the next bind falls back to the probe.
func (a *API) handleUpdateSettings(w http.ResponseWriter, r *http.Request) {
	var req settingsUpdateRequest
	if err := decodeJSON(w, r, &req); err != nil {
		a.badRequest(w, "请求格式不正确。", err)
		return
	}
	if req.PublicIP == nil && req.ExternalCheck == nil {
		writeError(w, http.StatusBadRequest, "E_BAD_REQUEST", "没有可保存的设置项。", "至少提交 public_ip 或 external_check。")
		return
	}
	ip := ""
	if req.PublicIP != nil {
		ip = strings.TrimSpace(*req.PublicIP)
		if ip != "" {
			parsed := net.ParseIP(ip)
			if parsed == nil || parsed.To4() == nil {
				writeError(w, http.StatusBadRequest, "E_BAD_REQUEST", "公网 IP 格式不正确。", "请填写 IPv4 地址，例如 1.2.3.4。")
				return
			}
			ip = parsed.To4().String()
		}
	}

	if err := a.config.Update(func(cfg *store.Config) error {
		if req.PublicIP != nil {
			cfg.Cloudflare.PublicIP = ip
		}
		if req.ExternalCheck != nil {
			cfg.Ports.ExternalCheck = *req.ExternalCheck
		}
		return nil
	}); err != nil {
		a.internalError(w, "save settings", err)
		return
	}

	// The fallback probe result is cached for the process lifetime; a manual
	// change must invalidate it so the new value wins. The port-probe cache is
	// keyed by public IP and port, so a toggle or address change clears it too.
	a.publicIPMu.Lock()
	a.publicIPCached = ""
	a.publicIPMu.Unlock()
	if a.probeCache != nil {
		a.probeCache.clear()
	}

	cfg := a.config.Snapshot()
	writeJSON(w, http.StatusOK, settingsView{
		PublicIP:      strings.TrimSpace(cfg.Cloudflare.PublicIP),
		ExternalCheck: cfg.Ports.ExternalCheck,
	})
}

// detectPublicIP returns an IPv4 address or a Chinese operator-facing reason.
// The raw probe error is intentionally not echoed: it may mention endpoints but
// carries no value for the operator.
func (a *API) detectPublicIP(ctx context.Context) (string, string) {
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()

	var (
		ip  string
		err error
	)
	if a.publicIPDetector != nil {
		ip, err = a.publicIPDetector(ctx)
	} else {
		ip, err = dns.DetectPublicIP(ctx, a.dnsHTTP)
	}
	if err != nil {
		return "", "自动探测公网 IP 失败（服务器可能无法访问探测服务），请手动填写。"
	}
	ip = strings.TrimSpace(ip)
	parsed := net.ParseIP(ip)
	if parsed == nil || parsed.To4() == nil {
		return "", "自动探测返回的不是有效 IPv4 地址，请手动填写。"
	}
	return parsed.To4().String(), ""
}
