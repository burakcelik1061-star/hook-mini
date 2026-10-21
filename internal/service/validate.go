package service

import (
	"encoding/json"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Limits. They mirror the CHECK constraints in the schema so a bad value is
// rejected here with a clear message; the database remains the backstop.
const (
	maxURLLen         = 2048
	maxDescriptionLen = 500
	maxEventTypeLen   = 128
	maxIdemKeyLen     = 255
	maxNameLen        = 200
	maxKeyNameLen     = 100
)

var eventTypeRE = regexp.MustCompile(`^[a-z0-9]+([._-][a-z0-9]+)*$`)

// ValidateEndpointURL checks that raw is a usable absolute http(s) URL.
//
// It checks syntax only. Whether the host may be contacted (private networks,
// loopback, cloud metadata addresses) is decided when a request is actually
// sent, because a hostname's address can change after it is validated.
func ValidateEndpointURL(raw string) error {
	const f = "url"
	if raw == "" {
		return invalid(f, "is required")
	}
	if len(raw) > maxURLLen {
		return invalid(f, "must be at most %d characters", maxURLLen)
	}
	if !utf8.ValidString(raw) {
		return invalid(f, "must be valid UTF-8")
	}
	// Any Unicode space or control character, not just ASCII whitespace: the
	// database's check uses the locale's idea of whitespace (U+2003 em space
	// and U+00A0 included), and a mismatch would turn a client mistake into a
	// server error.
	if strings.IndexFunc(raw, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) >= 0 {
		return invalid(f, "must not contain whitespace or control characters")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return invalid(f, "is not a valid URL")
	}
	if s := strings.ToLower(u.Scheme); s != "http" && s != "https" {
		return invalid(f, "must start with http:// or https://")
	}
	if u.Hostname() == "" {
		return invalid(f, "must include a host")
	}
	if u.User != nil {
		return invalid(f, "must not contain credentials")
	}
	if u.Fragment != "" || strings.Contains(raw, "#") {
		return invalid(f, "must not contain a fragment")
	}
	if p := u.Port(); p != "" {
		if n, err := strconv.Atoi(p); err != nil || n < 1 || n > 65535 {
			return invalid(f, "has an invalid port")
		}
	}
	return nil
}

func validateDescription(s string) error {
	if err := validateText("description", s); err != nil {
		return err
	}
	if utf8.RuneCountInString(s) > maxDescriptionLen {
		return invalid("description", "must be at most %d characters", maxDescriptionLen)
	}
	return nil
}

// validateText rejects what a Postgres text column cannot hold (invalid UTF-8,
// NUL) and control characters that have no place in a label. Tabs and line
// breaks are allowed.
func validateText(field, s string) error {
	if !utf8.ValidString(s) {
		return invalid(field, "must be valid UTF-8")
	}
	for _, r := range s {
		if unicode.IsControl(r) && r != '\t' && r != '\n' {
			return invalid(field, "must not contain control characters")
		}
	}
	return nil
}

// ValidateEventType checks the dotted, lower-case form such as "invoice.paid".
func ValidateEventType(t string) error {
	const f = "type"
	if t == "" {
		return invalid(f, "is required")
	}
	if len(t) > maxEventTypeLen {
		return invalid(f, "must be at most %d characters", maxEventTypeLen)
	}
	if !eventTypeRE.MatchString(t) {
		return invalid(f, "must be lower-case letters and digits separated by '.', '_' or '-' (for example invoice.paid)")
	}
	return nil
}

// validatePayload requires a JSON object. Webhook consumers expect to read
// named fields, and an object leaves room to add fields without breaking them.
func validatePayload(p json.RawMessage) error {
	const f = "payload"
	trimmed := strings.TrimSpace(string(p))
	if trimmed == "" || trimmed == "null" {
		return invalid(f, "is required")
	}
	if trimmed[0] != '{' {
		return invalid(f, "must be a JSON object")
	}
	// json.Valid accepts strings that are not valid UTF-8; Postgres does not.
	if !utf8.Valid(p) || !json.Valid(p) {
		return invalid(f, "is not valid JSON")
	}
	// What else jsonb refuses (a \u0000 escape, a lone surrogate, a number
	// beyond numeric's range) is decided by Postgres itself and reported by
	// dataError when the insert fails, so this check never drifts from the
	// database's.
	return nil
}

func validateIdempotencyKey(k *string) error {
	if k == nil {
		return nil
	}
	const f = "Idempotency-Key"
	if *k == "" {
		return invalid(f, "must not be empty")
	}
	if len(*k) > maxIdemKeyLen {
		return invalid(f, "must be at most %d characters", maxIdemKeyLen)
	}
	for i := 0; i < len(*k); i++ {
		if c := (*k)[i]; c < 0x21 || c > 0x7e {
			return invalid(f, "must contain only printable ASCII characters without spaces")
		}
	}
	return nil
}

func validateName(field, s string, max int) error {
	if err := validateText(field, s); err != nil {
		return err
	}
	n := utf8.RuneCountInString(strings.TrimSpace(s))
	if n == 0 {
		return invalid(field, "is required")
	}
	if n > max {
		return invalid(field, "must be at most %d characters", max)
	}
	return nil
}
