package orchestrator

import (
	"context"
	"regexp"
	"strings"
	"time"
)

// TS3 prints the first Server Admin privilege key in the container log. Two
// shapes exist depending on version/locale:
//
//	... token=AbC...
//	... Privilege Key: AbC...
//
// Both are scanned and the last match wins (the newest key is the valid one).
var tokenPatterns = []*regexp.Regexp{
	regexp.MustCompile(`token=([A-Za-z0-9+/=]{10,})`),
	regexp.MustCompile(`Privilege Key:\s*([A-Za-z0-9+/=]{10,})`),
}

// ExtractToken returns the last privilege key in the given log text.
func ExtractToken(logText string) (string, bool) {
	best := ""
	bestAt := -1
	for _, re := range tokenPatterns {
		for _, match := range re.FindAllStringSubmatchIndex(logText, -1) {
			if match[2] < 0 || match[3] < 0 {
				continue
			}
			if match[0] > bestAt {
				bestAt = match[0]
				best = logText[match[2]:match[3]]
			}
		}
	}
	return best, best != ""
}

// MaskToken renders a token for logs and lists: first 4 and last 4 characters,
// with everything in between hidden. Short values are fully hidden.
func MaskToken(token string) string {
	token = strings.TrimSpace(token)
	if token == "" {
		return ""
	}
	if len(token) <= 8 {
		return "****"
	}
	return token[:4] + "****" + token[len(token)-4:]
}

// PollToken calls logs up to attempts times and returns the first extracted
// token. A missing token is not an error: the instance is usable without it and
// the user can retry later by restarting.
func PollToken(ctx context.Context, logs func(context.Context) (string, error), attempts int, interval time.Duration) (string, error) {
	if attempts < 1 {
		attempts = 1
	}
	var lastErr error
	for attempt := 0; attempt < attempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-time.After(interval):
			}
		}
		text, err := logs(ctx)
		if err != nil {
			lastErr = err
			continue
		}
		if token, ok := ExtractToken(text); ok {
			return token, nil
		}
	}
	if lastErr != nil {
		return "", lastErr
	}
	return "", nil
}
