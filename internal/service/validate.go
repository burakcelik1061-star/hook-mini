package service

import (
	"encoding/json"
	"net/url"
	"regexp"
	"strconv"
	"strings"
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
	if strings.ContainsAny(raw, " \t\r\n") {
		return invalid(f, "must not contain whitespace")
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
	if utf8.RuneCountInString(s) > maxDescriptionLen {
		return invalid("description", "must be at most %d characters", maxDescriptionLen)
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
	if !json.Valid(p) {
		return invalid(f, "is not valid JSON")
	}
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
	n := utf8.RuneCountInString(strings.TrimSpace(s))
	if n == 0 {
		return invalid(field, "is required")
	}
	if n > max {
		return invalid(field, "must be at most %d characters", max)
	}
	return nil
}
