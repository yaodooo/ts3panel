package orchestrator

import "regexp"

// Container logs are shown in the instance detail page. Docker logs can contain
// the one-time ServerQuery admin token, a privilege key or a password (for
// example after a manual query login), so every log line is masked before it is
// returned to the API layer. Only the secret value is replaced; the surrounding
// text, key and separator stay readable for troubleshooting.
var (
	// Matches `token=value`, `password: value`, `api_token = value`,
	// `serveradmin_password="value"` and similar assignments. The value group
	// accepts quoted and unquoted forms and never spans a line.
	secretAssignmentRE = regexp.MustCompile(`(?i)\b((?:serveradmin_|query_)?password|passwd|token|secret|api[_-]?token|access[_-]?key[_-]?secret|secret[_-]?key)\b([ \t]*)([=:])([ \t]*)(("[^"]*")|('[^']*')|([^\s"']+))`)

	// TS3 prints the first server admin key as `Privilege Key: <key>`.
	privilegeKeyRE = regexp.MustCompile(`(?i)(privilege[ \t]+key[ \t]*:?[ \t]*)([A-Za-z0-9+/=_\-]{4,})`)
)

// MaskSecrets replaces credential-like values in a log excerpt with `****`.
// It is intentionally conservative about what it matches: a line is only
// changed when it contains a known key name followed by `=` or `:`.
func MaskSecrets(text string) string {
	text = privilegeKeyRE.ReplaceAllString(text, "${1}****")
	text = secretAssignmentRE.ReplaceAllString(text, "${1}${2}${3}${4}****")
	return text
}
