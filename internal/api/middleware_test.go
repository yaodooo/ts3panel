package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestCSRFRequiresHeaderOnWrites locks in iron rule 8: every state-changing
// /api request must carry X-Requested-With: ts3panel, while reads stay open.
func TestCSRFRequiresHeaderOnWrites(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	handler := (&API{}).csrf(next)

	cases := []struct {
		name       string
		method     string
		path       string
		header     string
		wantStatus int
		wantCode   string
	}{
		{name: "post without header", method: http.MethodPost, path: "/api/instances", wantStatus: http.StatusForbidden, wantCode: "E_CSRF"},
		{name: "delete without header", method: http.MethodDelete, path: "/api/instances/t1", wantStatus: http.StatusForbidden, wantCode: "E_CSRF"},
		{name: "post with header", method: http.MethodPost, path: "/api/instances", header: "ts3panel", wantStatus: http.StatusNoContent},
		{name: "get without header", method: http.MethodGet, path: "/api/instances", wantStatus: http.StatusNoContent},
		{name: "non-api post without header", method: http.MethodPost, path: "/setup", wantStatus: http.StatusNoContent},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, nil)
			if tc.header != "" {
				req.Header.Set("X-Requested-With", tc.header)
			}
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d", rec.Code, tc.wantStatus)
			}
			if tc.wantCode != "" {
				var body struct {
					Error struct {
						Code string `json:"code"`
					} `json:"error"`
				}
				if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
					t.Fatalf("decode: %v", err)
				}
				if body.Error.Code != tc.wantCode {
					t.Fatalf("code = %q, want %q", body.Error.Code, tc.wantCode)
				}
			}
		})
	}
}
