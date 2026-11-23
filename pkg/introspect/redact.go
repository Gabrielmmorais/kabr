package introspect

import (
	"regexp"
	"strings"
)

// Redacted replaces secrets in redacted DSNs and error messages.
const Redacted = "xxxxx"

var (
	// password=... / sslpassword=... in keyword/value DSNs; value may be
	// single-quoted with backslash escapes.
	kvPasswordRe = regexp.MustCompile(`(?i)\b((?:ssl)?password\s*=\s*)('(?:[^'\\]|\\.)*'?|\S*)`)
	// password=... / sslpassword=... in a URL query string.
	queryPasswordRe = regexp.MustCompile(`(?i)([?&](?:ssl)?password=)[^&#\s]*`)
	// scheme://user:<password>@ inside free text (error messages). Greedy up to
	// the last '@' of the token, so secrets containing '@' are covered.
	textURLPasswordRe = regexp.MustCompile(`([A-Za-z][A-Za-z0-9+.\-]*://[^:/\s@]*:)\S*@`)
)

// RedactDSN returns dsn with every password replaced by [Redacted]. It
// handles URL DSNs ("postgres://user:<password>@host/db?sslpassword=..."),
// including passwords containing unescaped '@', '/', ':' or invalid percent
// escapes, and keyword/value DSNs ("host=h password='s e c' ..."). It never
// fails: an unparsable DSN is scrubbed heuristically, erring on the side of
// redacting too much rather than leaking.
func RedactDSN(dsn string) string {
	if dsn == "" {
		return ""
	}
	scheme, rest, isURL := strings.Cut(dsn, "://")
	if !isURL {
		return kvPasswordRe.ReplaceAllString(dsn, "${1}"+Redacted)
	}
	// Locate the userinfo/host separator. Prefer the last '@' before the
	// query string; if there is none there, the password itself may contain
	// '?', so use the last '@' overall.
	head := rest
	if q := strings.IndexByte(rest, '?'); q >= 0 && strings.Contains(rest[:q], "@") {
		head = rest[:q]
	}
	if at := strings.LastIndexByte(head, '@'); at >= 0 {
		userinfo := rest[:at]
		if colon := strings.IndexByte(userinfo, ':'); colon >= 0 {
			rest = userinfo[:colon+1] + Redacted + rest[at:]
		}
	}
	out := scheme + "://" + rest
	return queryPasswordRe.ReplaceAllString(out, "${1}"+Redacted)
}

// RedactText scrubs credentials from free text such as an error message:
// URL userinfo passwords, keyword/value passwords, and any of the given
// literal secrets (empty secrets are ignored).
func RedactText(s string, secrets ...string) string {
	for _, sec := range secrets {
		if sec != "" {
			s = strings.ReplaceAll(s, sec, Redacted)
		}
	}
	s = textURLPasswordRe.ReplaceAllString(s, "${1}"+Redacted+"@")
	s = queryPasswordRe.ReplaceAllString(s, "${1}"+Redacted)
	return kvPasswordRe.ReplaceAllString(s, "${1}"+Redacted)
}

// redactedError carries a scrubbed message. Unwrap still exposes the cause
// so errors.Is/As keep working (context.Canceled, driver error types); the
// drivers used by Kabr already omit passwords from their own messages, the
// scrubbed message is defense in depth.
type redactedError struct {
	msg string
	err error
}

func (e *redactedError) Error() string { return e.msg }
func (e *redactedError) Unwrap() error { return e.err }

// SanitizeError returns err with its message scrubbed by [RedactText],
// prefixed by prefix (when non-empty, as "prefix: message"). It returns nil
// for a nil err.
func SanitizeError(prefix string, err error, secrets ...string) error {
	if err == nil {
		return nil
	}
	msg := RedactText(err.Error(), secrets...)
	if prefix != "" {
		msg = prefix + ": " + msg
	}
	return &redactedError{msg: msg, err: err}
}
