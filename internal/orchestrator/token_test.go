package orchestrator

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestExtractTokenForms(t *testing.T) {
	cases := []struct {
		name string
		log  string
		want string
	}{
		{
			name: "token equals",
			log:  "2026-10-21 12:00:00.000000|INFO|ServerAdmin|token=AbCdEfGhIjKlMnOpQrStUvWx\n",
			want: "AbCdEfGhIjKlMnOpQrStUvWx",
		},
		{
			name: "privilege key",
			log:  "Privilege Key: 2q3r4s5t6u7v8w9x0y1z2a3b4c\n",
			want: "2q3r4s5t6u7v8w9x0y1z2a3b4c",
		},
		{
			name: "last match wins",
			log: "token=AAAAAAAAAAAA\n" +
				"token=BBBBBBBBBBBB\n",
			want: "BBBBBBBBBBBB",
		},
		{
			name: "last across patterns wins",
			log: "Privilege Key: CCCCCCCCCCCC\n" +
				"token=DDDDDDDDDDDD\n",
			want: "DDDDDDDDDDDD",
		},
		{
			name: "base64 padding",
			log:  "token=AbCdEfGhIj+/=\n",
			want: "AbCdEfGhIj+/=",
		},
		{
			name: "too short ignored",
			log:  "token=short\nPrivilege Key: alsoShort\n",
			want: "",
		},
		{
			name: "no token",
			log:  "server started\nlistening on 9987\n",
			want: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := ExtractToken(tc.log)
			if got != tc.want {
				t.Fatalf("token = %q, want %q", got, tc.want)
			}
			if ok != (tc.want != "") {
				t.Fatalf("ok = %v, want %v", ok, tc.want != "")
			}
		})
	}
}

func TestMaskToken(t *testing.T) {
	cases := map[string]string{
		"":                       "",
		"short":                  "****",
		"12345678":               "****",
		"123456789":              "1234****6789",
		"AbCdEfGhIjKlMnOpQrStUv": "AbCd****StUv",
	}
	for input, want := range cases {
		if got := MaskToken(input); got != want {
			t.Fatalf("MaskToken(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestPollTokenRetriesUntilAvailable(t *testing.T) {
	calls := 0
	logs := func(ctx context.Context) (string, error) {
		calls++
		if calls < 3 {
			return "starting up", nil
		}
		return "Privilege Key: ZZZZZZZZZZZZ", nil
	}
	token, err := PollToken(context.Background(), logs, 6, time.Millisecond)
	if err != nil {
		t.Fatalf("PollToken: %v", err)
	}
	if token != "ZZZZZZZZZZZZ" {
		t.Fatalf("token = %q", token)
	}
	if calls != 3 {
		t.Fatalf("calls = %d, want 3", calls)
	}
}

func TestPollTokenGivesUpWithoutError(t *testing.T) {
	logs := func(ctx context.Context) (string, error) { return "no token here", nil }
	token, err := PollToken(context.Background(), logs, 2, time.Millisecond)
	if err != nil {
		t.Fatalf("PollToken: %v", err)
	}
	if token != "" {
		t.Fatalf("token = %q, want empty", token)
	}
}

func TestPollTokenStopsOnContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	logs := func(ctx context.Context) (string, error) {
		return strings.Repeat("x", 1), nil
	}
	_, err := PollToken(ctx, logs, 3, time.Second)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}
