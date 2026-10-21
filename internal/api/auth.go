package api

import (
	"errors"
	"net/http"
	"regexp"
	"strings"
	"time"
	"unicode"

	"golang.org/x/crypto/bcrypt"

	"ts3panel/internal/model"
	"ts3panel/internal/store"
)

const bcryptCost = 12

var (
	usernameRE         = regexp.MustCompile(`^[A-Za-z0-9_-]{3,32}$`)
	slugRE             = regexp.MustCompile(`^[a-z0-9-]{1,20}$`)
	errRecoveryInvalid = errors.New("recovery code invalid or used")
)

type loginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

type loginResponse struct {
	OK       bool   `json:"ok"`
	Username string `json:"username"`
}

type sessionResponse struct {
	Authenticated  bool   `json:"authenticated"`
	Username       string `json:"username,omitempty"`
	SetupCompleted bool   `json:"setup_completed"`
}

type forgotRequest struct {
	Username     string `json:"username"`
	RecoveryCode string `json:"recovery_code"`
	NewPassword  string `json:"new_password"`
}

type forgotResponse struct {
	OK                     bool   `json:"ok"`
	Message                string `json:"message"`
	RemainingRecoveryCodes int    `json:"remaining_recovery_codes"`
	LowRecoveryCodes       bool   `json:"low_recovery_codes"`
}

func validateUsername(username string) error {
	if !usernameRE.MatchString(username) {
		return errors.New("用户名需为 3–32 位，仅限字母、数字、下划线和连字符。")
	}
	return nil
}

// validateSlug follows the instance-ID rule in §8.3: lowercase letters, digits
// and hyphens, 1–20 characters. It is used by M2 instance creation.
func validateSlug(slug string) error {
	if !slugRE.MatchString(slug) {
		return errors.New("实例 ID 必须为 1–20 位小写字母、数字或连字符。")
	}
	return nil
}

func validatePassword(password string) error {
	if len([]rune(password)) < 10 {
		return errors.New("密码至少需要 10 位。")
	}
	// bcrypt only consumes the first 72 bytes; reject longer input instead of
	// silently truncating it.
	if len([]byte(password)) > 72 {
		return errors.New("密码过长，最多 72 字节。")
	}
	hasLetter, hasDigit := false, false
	for _, r := range password {
		switch {
		case unicode.IsLetter(r):
			hasLetter = true
		case unicode.IsDigit(r):
			hasDigit = true
		}
	}
	if !hasLetter || !hasDigit {
		return errors.New("密码必须同时包含字母和数字。")
	}
	return nil
}

func hashPassword(password string) ([]byte, error) {
	return bcrypt.GenerateFromPassword([]byte(password), bcryptCost)
}

func verifyPassword(hash, password string) bool {
	if hash == "" {
		return false
	}
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) == nil
}

func loginLimitKey(r *http.Request, username string) string {
	return "login|" + clientIP(r) + "|" + strings.ToLower(strings.TrimSpace(username))
}

func (a *API) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req loginRequest
	if err := decodeJSON(w, r, &req); err != nil {
		a.badRequest(w, "请求格式不正确。", err)
		return
	}
	username := strings.TrimSpace(req.Username)
	if username == "" || len(username) > 64 || req.Password == "" {
		writeError(w, http.StatusBadRequest, "E_BAD_REQUEST",
			"请输入用户名和密码。", "")
		return
	}

	key := loginLimitKey(r, username)
	if locked, retry := a.loginLimiter.Check(key, time.Now()); locked {
		writeRateLimit(w, retry)
		return
	}

	st, err := a.state.Snapshot()
	if err != nil {
		a.internalError(w, "login load state", err)
		return
	}

	valid := false
	if st.User != nil && strings.EqualFold(st.User.Username, username) {
		valid = verifyPassword(st.User.PasswordHash, req.Password)
	} else {
		// Equalize the common case: unknown usernames still pay one bcrypt
		// comparison so response timing does not reveal account existence.
		_ = bcrypt.CompareHashAndPassword(a.dummyHash, []byte(req.Password))
	}

	if !valid {
		if locked, retry := a.loginLimiter.Fail(key, time.Now()); locked {
			writeRateLimit(w, retry)
			return
		}
		writeError(w, http.StatusUnauthorized, "E_AUTH",
			"用户名或密码错误。", "")
		return
	}

	a.loginLimiter.Reset(key)
	if err := a.newSession(w, r, st.User.Username); err != nil {
		a.internalError(w, "create login session", err)
		return
	}
	writeJSON(w, http.StatusOK, loginResponse{OK: true, Username: st.User.Username})
}

func (a *API) handleLogout(w http.ResponseWriter, r *http.Request) {
	if sid, ok := sessionID(r); ok {
		a.sessions.Delete(sid)
	}
	clearSessionCookie(w)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (a *API) handleSession(w http.ResponseWriter, r *http.Request) {
	st, err := a.state.Snapshot()
	if err != nil {
		a.internalError(w, "session load state", err)
		return
	}
	sid, username, ok := a.currentSession(r)
	if ok {
		setSessionCookie(w, r, sid)
		writeJSON(w, http.StatusOK, sessionResponse{
			Authenticated:  true,
			Username:       username,
			SetupCompleted: st.SetupCompleted,
		})
		return
	}
	writeJSON(w, http.StatusOK, sessionResponse{
		Authenticated:  false,
		SetupCompleted: st.SetupCompleted,
	})
}

func (a *API) handleForgotPassword(w http.ResponseWriter, r *http.Request) {
	var req forgotRequest
	if err := decodeJSON(w, r, &req); err != nil {
		a.badRequest(w, "请求格式不正确。", err)
		return
	}
	username := strings.TrimSpace(req.Username)

	ipKey := "forgot|" + clientIP(r)
	if locked, retry := a.forgotLimiter.Check(ipKey, time.Now()); locked {
		writeRateLimit(w, retry)
		return
	}
	if len(req.Username) > 64 || len(req.RecoveryCode) > 128 {
		a.failRecovery(w, ipKey)
		return
	}
	code := normalizeRecoveryCode(req.RecoveryCode)
	if username == "" || code == "" {
		a.failRecovery(w, ipKey)
		return
	}
	if err := validatePassword(req.NewPassword); err != nil {
		writeError(w, http.StatusBadRequest, "E_BAD_REQUEST", err.Error(), "")
		return
	}

	st, err := a.state.Snapshot()
	if err != nil {
		a.internalError(w, "forgot load state", err)
		return
	}

	matchIdx := -1
	matchHash := ""
	if st.User != nil && strings.EqualFold(st.User.Username, username) {
		for i := range st.RecoveryCodes {
			rc := &st.RecoveryCodes[i]
			if rc.Used || rc.Hash == "" {
				continue
			}
			if bcrypt.CompareHashAndPassword([]byte(rc.Hash), []byte(code)) == nil {
				matchIdx = i
				matchHash = rc.Hash
				break
			}
		}
	}
	if matchIdx < 0 {
		a.failRecovery(w, ipKey)
		return
	}

	newHash, err := hashPassword(req.NewPassword)
	if err != nil {
		a.internalError(w, "hash new password", err)
		return
	}

	usedAt := time.Now().UTC()
	err = a.state.Update(func(cur *store.State) error {
		if cur.User == nil || !strings.EqualFold(cur.User.Username, username) {
			return errRecoveryInvalid
		}
		if matchIdx >= len(cur.RecoveryCodes) {
			return errRecoveryInvalid
		}
		rc := &cur.RecoveryCodes[matchIdx]
		if rc.Used || rc.Hash != matchHash {
			return errRecoveryInvalid
		}
		rc.Used = true
		rc.UsedAt = &usedAt
		cur.User.PasswordHash = string(newHash)
		return nil
	})
	if err != nil {
		if errors.Is(err, errRecoveryInvalid) {
			a.failRecovery(w, ipKey)
			return
		}
		a.internalError(w, "forgot update state", err)
		return
	}

	a.forgotLimiter.Reset(ipKey)
	a.sessions.DeleteAll()
	clearSessionCookie(w)

	remaining := 0
	if fresh, err := a.state.Snapshot(); err == nil {
		remaining = countUnusedRecoveryCodes(fresh.RecoveryCodes)
	} else {
		remaining = countUnusedRecoveryCodes(st.RecoveryCodes) - 1
		if remaining < 0 {
			remaining = 0
		}
	}

	msg := "密码已重置，请使用新密码登录。"
	if remaining < 3 {
		msg += " 恢复码剩余不足 3 个，请运行 ts3panel reset-password 重新生成。"
	}
	writeJSON(w, http.StatusOK, forgotResponse{
		OK:                     true,
		Message:                msg,
		RemainingRecoveryCodes: remaining,
		LowRecoveryCodes:       remaining < 3,
	})
}

func (a *API) failRecovery(w http.ResponseWriter, ipKey string) {
	if locked, retry := a.forgotLimiter.Fail(ipKey, time.Now()); locked {
		writeRateLimit(w, retry)
		return
	}
	writeError(w, http.StatusBadRequest, "E_RECOVERY_INVALID",
		"用户名或恢复码无效，或该恢复码已使用。", "")
}

func countUnusedRecoveryCodes(codes []model.RecoveryCode) int {
	n := 0
	for i := range codes {
		if !codes[i].Used {
			n++
		}
	}
	return n
}
