package api

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode"

	"ts3panel/internal/model"
	"ts3panel/internal/store"
)

const (
	recoveryCodeCount = 8
	// No 0/O/1/I/l: these are too easy to transcribe incorrectly.
	recoveryAlphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"
)

var (
	errSetupDone  = errors.New("setup already completed")
	errSetupToken = errors.New("setup token invalid or expired")
)

var providerDefaultLabels = map[string]string{
	"cloudflare": "Cloudflare",
	"aliyun":     "阿里云 DNS",
	"dnspod":     "DNSPod",
	"manual":     "手动解析",
}

type setupRequest struct {
	Username   string           `json:"username"`
	Password   string           `json:"password"`
	PublicIP   string           `json:"public_ip"`
	SetupToken string           `json:"setup_token"`
	Provider   *providerRequest `json:"provider"`
}

type providerRequest struct {
	Type  string            `json:"type"`
	Label string            `json:"label"`
	Zone  string            `json:"zone"`
	Creds map[string]string `json:"creds"`
}

type setupResponse struct {
	OK            bool     `json:"ok"`
	RecoveryCodes []string `json:"recovery_codes"`
	ShowOnce      bool     `json:"show_once"`
	Notice        string   `json:"notice"`
	Reset         bool     `json:"reset"`
	AutoLogin     bool     `json:"auto_login"`
}

type setupStateResponse struct {
	SetupCompleted      bool       `json:"setup_completed"`
	ResetAllowed        bool       `json:"reset_allowed"`
	Username            string     `json:"username,omitempty"`
	PublicIP            string     `json:"public_ip,omitempty"`
	SetupDNSSkipped     bool       `json:"setup_dns_skipped,omitempty"`
	SetupTokenExpiresAt *time.Time `json:"setup_token_expires_at,omitempty"`
}

// handleSetupState is readable before setup and is how the setup page detects a
// valid CLI reset token. The query token is never logged.
func (a *API) handleSetupState(w http.ResponseWriter, r *http.Request) {
	st, err := a.state.Snapshot()
	if err != nil {
		a.internalError(w, "setup state load", err)
		return
	}
	cfg := a.config.Snapshot()
	resp := setupStateResponse{
		SetupCompleted:  st.SetupCompleted,
		PublicIP:        cfg.Cloudflare.PublicIP,
		SetupDNSSkipped: st.SetupDNSSkipped,
	}
	if st.SetupCompleted && st.User != nil && st.SetupToken != nil {
		now := time.Now().UTC()
		if st.SetupToken.Valid(r.URL.Query().Get("token"), now) {
			expires := st.SetupToken.ExpiresAt
			resp.ResetAllowed = true
			resp.Username = st.User.Username
			resp.SetupTokenExpiresAt = &expires
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleSetup creates the administrator and the first recovery-code set. With a
// valid setup_token it also supports the root CLI reset flow, replacing the
// account and codes in one atomic state write.
func (a *API) handleSetup(w http.ResponseWriter, r *http.Request) {
	var req setupRequest
	if err := decodeJSON(w, r, &req); err != nil {
		a.badRequest(w, "请求格式不正确。", err)
		return
	}
	token := strings.TrimSpace(req.SetupToken)

	a.setupMu.Lock()
	defer a.setupMu.Unlock()

	st, err := a.state.Snapshot()
	if err != nil {
		a.internalError(w, "setup load state", err)
		return
	}

	// Setup is only tokenless on a loopback listener. A non-loopback panel
	// requires the one-time token produced by `ts3panel reset-password`.
	if st.SetupCompleted {
		if token == "" {
			writeSetupDone(w)
			return
		}
		if st.SetupToken == nil || !st.SetupToken.Valid(token, time.Now().UTC()) {
			writeSetupTokenInvalid(w)
			return
		}
	} else if !a.listenLoopback {
		if st.SetupToken == nil || !st.SetupToken.Valid(token, time.Now().UTC()) {
			writeSetupTokenInvalid(w)
			return
		}
	}
	isReset := st.SetupCompleted

	username := strings.TrimSpace(req.Username)
	if err := validateUsername(username); err != nil {
		writeError(w, http.StatusBadRequest, "E_BAD_REQUEST", err.Error(), "")
		return
	}
	if err := validatePassword(req.Password); err != nil {
		writeError(w, http.StatusBadRequest, "E_BAD_REQUEST", err.Error(), "")
		return
	}

	var publicIP string
	var provider *store.ProviderConfig
	if !isReset {
		publicIP, err = normalizePublicIP(req.PublicIP)
		if err != nil {
			writeError(w, http.StatusBadRequest, "E_BAD_REQUEST", err.Error(), "")
			return
		}
		provider, err = a.buildProviderConfig(req.Provider)
		if err != nil {
			writeError(w, http.StatusBadRequest, "E_BAD_REQUEST", err.Error(), "")
			return
		}
	}

	codes, err := generateRecoveryCodes()
	if err != nil {
		a.internalError(w, "generate recovery codes", err)
		return
	}

	type passwordResult struct {
		hash []byte
		err  error
	}
	pwCh := make(chan passwordResult, 1)
	go func() {
		hash, hashErr := hashPassword(req.Password)
		pwCh <- passwordResult{hash: hash, err: hashErr}
	}()

	codeHashes, codeErr := hashRecoveryCodes(codes)
	pw := <-pwCh
	if codeErr != nil {
		a.internalError(w, "hash recovery codes", codeErr)
		return
	}
	if pw.err != nil {
		a.internalError(w, "hash setup password", pw.err)
		return
	}

	var oldConfig store.Config
	configChanged := false
	if !isReset && (provider != nil || publicIP != "") {
		oldConfig = a.config.Snapshot()
		next := oldConfig
		// Keep oldConfig pristine for rollback: UpsertProvider mutates the
		// provider slice in place when updating an existing entry.
		next.Providers = append([]store.ProviderConfig(nil), oldConfig.Providers...)
		if publicIP != "" {
			next.Cloudflare.PublicIP = publicIP
		}
		if provider != nil {
			next.UpsertProvider(*provider)
		}
		if err := a.config.Save(next); err != nil {
			a.internalError(w, "persist setup config", err)
			return
		}
		configChanged = true
	}

	appliedReset := false
	now := time.Now().UTC()
	err = a.state.Update(func(cur *store.State) error {
		if cur.SetupCompleted {
			if !cur.SetupToken.Valid(token, now) {
				if isReset {
					return errSetupToken
				}
				return errSetupDone
			}
			appliedReset = true
		} else if !a.listenLoopback {
			if !cur.SetupToken.Valid(token, now) {
				return errSetupToken
			}
			appliedReset = false
		} else {
			appliedReset = false
		}

		cur.User = &model.User{
			Username:     username,
			PasswordHash: string(pw.hash),
			CreatedAt:    now,
		}
		cur.RecoveryCodes = make([]model.RecoveryCode, len(codeHashes))
		for i := range codeHashes {
			cur.RecoveryCodes[i] = model.RecoveryCode{Hash: codeHashes[i]}
		}
		cur.SetupCompleted = true
		cur.SetupDNSSkipped = false
		cur.SetupToken = nil
		return nil
	})
	if err != nil {
		if configChanged {
			if rbErr := a.config.Save(oldConfig); rbErr != nil {
				slog.Error("setup config rollback failed", "err", rbErr)
			}
		}
		switch {
		case errors.Is(err, errSetupDone):
			writeSetupDone(w)
		case errors.Is(err, errSetupToken):
			writeSetupTokenInvalid(w)
		default:
			a.internalError(w, "setup update state", err)
		}
		return
	}

	// A reset must log out every old session; the initial setup normally has no
	// sessions, so this is a no-op there.
	a.sessions.DeleteAll()
	autoLogin := true
	if err := a.newSession(w, r, username); err != nil {
		slog.Error("setup auto-login failed", "err", err)
		autoLogin = false
	}

	writeJSON(w, http.StatusOK, setupResponse{
		OK:            true,
		RecoveryCodes: codes,
		ShowOnce:      true,
		Notice:        "恢复码仅显示这一次，请立即保存；关闭页面后将无法再次查看。",
		Reset:         appliedReset,
		AutoLogin:     autoLogin,
	})
}

// handleSetupSkipDNS records the wizard's manual-DNS choice. M1 has no DNS
// implementation; the flag only lets the wizard resume correctly.
func (a *API) handleSetupSkipDNS(w http.ResponseWriter, r *http.Request) {
	st, err := a.state.Snapshot()
	if err != nil {
		a.internalError(w, "skip-dns load state", err)
		return
	}
	if st.SetupCompleted {
		writeSetupDone(w)
		return
	}
	err = a.state.Update(func(cur *store.State) error {
		if cur.SetupCompleted {
			return errSetupDone
		}
		cur.SetupDNSSkipped = true
		return nil
	})
	if err != nil {
		if errors.Is(err, errSetupDone) {
			writeSetupDone(w)
			return
		}
		a.internalError(w, "skip-dns update state", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "mode": "manual"})
}

func writeSetupDone(w http.ResponseWriter) {
	writeError(w, http.StatusConflict, "E_SETUP_DONE",
		"面板已完成初始化。",
		"如需重新设置管理员账号，请在服务器运行 ts3panel reset-password。")
}

func writeSetupTokenInvalid(w http.ResponseWriter) {
	writeError(w, http.StatusForbidden, "E_SETUP_TOKEN",
		"重置链接无效或已过期。",
		"请在服务器重新运行 ts3panel reset-password。")
}

func (a *API) buildProviderConfig(req *providerRequest) (*store.ProviderConfig, error) {
	if req == nil {
		return nil, nil
	}
	typ := strings.ToLower(strings.TrimSpace(req.Type))
	switch typ {
	case "cloudflare", "aliyun", "dnspod", "manual":
	default:
		return nil, errors.New("不支持的解析平台。")
	}
	zone := store.NormalizeDomain(req.Zone)
	if !store.ValidDomain(zone) {
		return nil, errors.New("主域名格式不正确，例如 example.com。")
	}
	label := strings.TrimSpace(req.Label)
	if label == "" {
		label = providerDefaultLabels[typ]
	}
	if len([]rune(label)) > 80 {
		return nil, errors.New("平台显示名称过长（最多 80 个字符）。")
	}

	creds := map[string]string{}
	switch typ {
	case "cloudflare":
		creds["api_token"] = strings.TrimSpace(req.Creds["api_token"])
		if creds["api_token"] == "" {
			return nil, errors.New("请填写 Cloudflare API Token。")
		}
	case "aliyun":
		creds["access_key_id"] = strings.TrimSpace(req.Creds["access_key_id"])
		creds["access_key_secret"] = strings.TrimSpace(req.Creds["access_key_secret"])
		if creds["access_key_id"] == "" || creds["access_key_secret"] == "" {
			return nil, errors.New("请填写阿里云 AccessKey ID 与 AccessKey Secret。")
		}
	case "dnspod":
		creds["secret_id"] = strings.TrimSpace(req.Creds["secret_id"])
		creds["secret_key"] = strings.TrimSpace(req.Creds["secret_key"])
		if creds["secret_id"] == "" || creds["secret_key"] == "" {
			return nil, errors.New("请填写 DNSPod SecretId 与 SecretKey。")
		}
	}

	provider := &store.ProviderConfig{
		Type:    typ,
		Label:   label,
		Zone:    zone,
		Enabled: true,
		Default: true,
	}
	if len(creds) > 0 {
		if a.cipher == nil {
			return nil, errors.New("凭据加密组件未初始化。")
		}
		plain, err := json.Marshal(creds)
		if err != nil {
			return nil, fmt.Errorf("encode provider credentials: %w", err)
		}
		encoded, err := a.cipher.Encrypt(plain)
		if err != nil {
			return nil, fmt.Errorf("encrypt provider credentials: %w", err)
		}
		provider.CredsEnc = encoded
	}
	return provider, nil
}

// isLoopbackListen reports whether the configured listen address is reachable
// only from the local host. Tokenless first-run setup is allowed only then.
func isLoopbackListen(listen string) bool {
	host, _, err := net.SplitHostPort(strings.TrimSpace(listen))
	if err != nil {
		return false
	}
	host = strings.TrimSpace(host)
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func normalizePublicIP(input string) (string, error) {
	input = strings.TrimSpace(input)
	if input == "" {
		return "", nil
	}
	ip := net.ParseIP(input)
	if ip == nil || ip.To4() == nil {
		return "", errors.New("公网 IP 必须是合法的 IPv4 地址，例如 1.2.3.4。")
	}
	return ip.To4().String(), nil
}

func generateRecoveryCodes() ([]string, error) {
	seen := make(map[string]struct{}, recoveryCodeCount)
	codes := make([]string, 0, recoveryCodeCount)
	alphabetLen := big.NewInt(int64(len(recoveryAlphabet)))
	for len(codes) < recoveryCodeCount {
		var b strings.Builder
		for i := 0; i < 8; i++ {
			n, err := rand.Int(rand.Reader, alphabetLen)
			if err != nil {
				return nil, fmt.Errorf("generate recovery code: %w", err)
			}
			b.WriteByte(recoveryAlphabet[n.Int64()])
		}
		raw := b.String()
		code := raw[:4] + "-" + raw[4:]
		if _, exists := seen[code]; exists {
			continue
		}
		seen[code] = struct{}{}
		codes = append(codes, code)
	}
	return codes, nil
}

func hashRecoveryCodes(codes []string) ([]string, error) {
	hashes := make([]string, len(codes))
	errs := make([]error, len(codes))
	var wg sync.WaitGroup
	for i := range codes {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			hash, err := hashPassword(codes[i])
			hashes[i] = string(hash)
			errs[i] = err
		}(i)
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			return nil, err
		}
	}
	return hashes, nil
}

// normalizeRecoveryCode accepts the canonical XXXX-XXXX form plus common copy
// variants (lowercase, spaces, missing hyphen) and returns "" for anything that
// cannot be a generated code.
func normalizeRecoveryCode(input string) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(strings.TrimSpace(input)) {
		if r == '-' || unicode.IsSpace(r) {
			continue
		}
		if !strings.ContainsRune(recoveryAlphabet, r) {
			return ""
		}
		b.WriteRune(r)
	}
	if b.Len() != 8 {
		return ""
	}
	raw := b.String()
	return raw[:4] + "-" + raw[4:]
}
