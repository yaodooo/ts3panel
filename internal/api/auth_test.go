package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ts3panel/internal/model"
	"ts3panel/internal/store"
)

func newAuthTestAPI(t *testing.T) (*API, *store.StateStore) {
	t.Helper()

	dir := t.TempDir()
	configStore, _, err := store.LoadConfig(filepath.Join(dir, "config.toml"))
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	stateStore, err := store.OpenState(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatalf("open state: %v", err)
	}

	passwordHash, err := hashPassword("AdminPassw0rd1")
	if err != nil {
		t.Fatalf("hash password: %v", err)
	}
	recoveryHash, err := hashPassword("ABCD-EFGH")
	if err != nil {
		t.Fatalf("hash recovery code: %v", err)
	}
	now := time.Now().UTC()
	if err := stateStore.Update(func(st *store.State) error {
		st.SetupCompleted = true
		st.User = &model.User{
			Username:     "admin",
			PasswordHash: string(passwordHash),
			CreatedAt:    now,
		}
		st.RecoveryCodes = []model.RecoveryCode{{Hash: string(recoveryHash)}}
		return nil
	}); err != nil {
		t.Fatalf("seed state: %v", err)
	}

	apiHandler, err := New(Options{Config: configStore, State: stateStore, Version: "test"})
	if err != nil {
		t.Fatalf("new api: %v", err)
	}
	return apiHandler, stateStore
}

func postForgot(t *testing.T, handler http.Handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/password/forgot", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Requested-With", "ts3panel")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func TestRecoveryCodeSingleUse(t *testing.T) {
	apiHandler, stateStore := newAuthTestAPI(t)
	handler := apiHandler.Handler()

	first := postForgot(t, handler,
		`{"username":"admin","recovery_code":"ABCD-EFGH","new_password":"NewPassw0rd1"}`)
	if first.Code != http.StatusOK {
		t.Fatalf("first forgot status = %d, want 200; body=%s", first.Code, first.Body.String())
	}
	var firstBody forgotResponse
	if err := json.Unmarshal(first.Body.Bytes(), &firstBody); err != nil {
		t.Fatalf("decode first response: %v", err)
	}
	if !firstBody.OK || firstBody.RemainingRecoveryCodes != 0 {
		t.Fatalf("first response = %+v, want ok with 0 remaining", firstBody)
	}

	st, err := stateStore.Snapshot()
	if err != nil {
		t.Fatalf("snapshot after first reset: %v", err)
	}
	if len(st.RecoveryCodes) != 1 || !st.RecoveryCodes[0].Used {
		t.Fatalf("recovery code was not marked used: %+v", st.RecoveryCodes)
	}

	second := postForgot(t, handler,
		`{"username":"admin","recovery_code":"ABCD-EFGH","new_password":"AnotherPassw0rd2"}`)
	if second.Code != http.StatusBadRequest {
		t.Fatalf("reused code status = %d, want 400; body=%s", second.Code, second.Body.String())
	}
	var secondBody apiErrorEnvelope
	if err := json.Unmarshal(second.Body.Bytes(), &secondBody); err != nil {
		t.Fatalf("decode second response: %v", err)
	}
	if secondBody.Error.Code != "E_RECOVERY_INVALID" {
		t.Fatalf("reused code error = %q, want E_RECOVERY_INVALID", secondBody.Error.Code)
	}
}

func TestSetupTokenExpiry(t *testing.T) {
	token, record, err := store.NewSetupToken(5 * time.Minute)
	if err != nil {
		t.Fatalf("new setup token: %v", err)
	}
	now := time.Now().UTC()
	if !record.Valid(token, now) {
		t.Fatal("fresh setup token is not valid")
	}
	if record.Valid("wrong-token", now) {
		t.Fatal("setup token accepted a wrong value")
	}
	expired := record
	expired.ExpiresAt = now.Add(-time.Second)
	if expired.Valid(token, now) {
		t.Fatal("expired setup token is still valid")
	}
	if record.Valid("", now) {
		t.Fatal("empty setup token accepted")
	}
}

func TestValidateUsername(t *testing.T) {
	valid := []string{"abc", "yao_01", "A-b_3", "a" + strings.Repeat("b", 31)}
	for _, username := range valid {
		if err := validateUsername(username); err != nil {
			t.Errorf("validateUsername(%q) = %v, want nil", username, err)
		}
	}

	invalid := []string{"", "ab", "a b", "admin!", "用户", strings.Repeat("a", 33)}
	for _, username := range invalid {
		if err := validateUsername(username); err == nil {
			t.Errorf("validateUsername(%q) = nil, want error", username)
		}
	}
}

func TestValidateSlug(t *testing.T) {
	valid := []string{"a", "ts1", "abc-123", strings.Repeat("a", 20)}
	for _, slug := range valid {
		if err := validateSlug(slug); err != nil {
			t.Errorf("validateSlug(%q) = %v, want nil", slug, err)
		}
	}

	invalid := []string{"", "TS1", "ts_1", "ts 1", "ts/1", strings.Repeat("a", 21)}
	for _, slug := range invalid {
		if err := validateSlug(slug); err == nil {
			t.Errorf("validateSlug(%q) = nil, want error", slug)
		}
	}
}
