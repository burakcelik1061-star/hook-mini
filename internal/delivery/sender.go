package delivery

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
)

const (
	userAgent = "HookMini/1.0"

	// maxSnippetRunes is how much of a response body is kept for the delivery
	// history. It matches the CHECK constraint on delivery_attempts.
	maxSnippetRunes = 1024
	// maxDrainBytes is how much of a response body is read at most. Reading
	// (rather than abandoning) a small body lets the connection be reused; a
	// huge body is cut off so an endpoint cannot make the worker download it.
	maxDrainBytes = 64 << 10
	// maxErrorRunes bounds error text stored in the database.
	maxErrorRunes = 500
)

// envelope is the JSON body sent to an endpoint. It wraps the tenant's
// payload with the metadata every receiver needs.
type envelope struct {
	ID        uuid.UUID       `json:"id"`
	Type      string          `json:"type"`
	CreatedAt time.Time       `json:"created_at"`
	Data      json.RawMessage `json:"data"`
}

// message is what one attempt sends.
type message struct {
	url     string
	eventID uuid.UUID
	body    []byte
}

func newMessage(url string, eventID uuid.UUID, eventType string, createdAt time.Time, payload json.RawMessage) (message, error) {
	body, err := json.Marshal(envelope{ID: eventID, Type: eventType, CreatedAt: createdAt.UTC(), Data: payload})
	if err != nil {
		return message{}, fmt.Errorf("encode envelope: %w", err)
	}
	return message{url: url, eventID: eventID, body: body}, nil
}

// result is what came back from one attempt.
type result struct {
	started    time.Time
	duration   time.Duration
	statusCode int    // 0 when no HTTP response arrived
	snippet    string // start of the response body, safe to store
	errText    string // empty only for a 2xx response
	retryAfter time.Duration
}

func (r result) ok() bool { return r.errText == "" }

// sender performs HTTP attempts.
type sender struct {
	client  *http.Client
	timeout time.Duration
	now     func() time.Time
}

func newSender(transport http.RoundTripper, timeout time.Duration) *sender {
	if transport == nil {
		transport = defaultTransport()
	}
	return &sender{
		client: &http.Client{
			Transport: transport,
			// Redirects are never followed. A webhook URL is configured
			// explicitly; following a redirect would let an endpoint steer
			// the worker to any other address (including internal ones), and
			// would turn a POST into a GET. A 3xx is reported as a failure.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		timeout: timeout,
		now:     time.Now,
	}
}

// defaultTransport is a plain transport that ignores HTTP_PROXY and friends:
// webhook traffic goes straight to the endpoint, never through a proxy picked
// up from the worker's environment.
func defaultTransport() *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.Proxy = nil
	t.MaxIdleConnsPerHost = 16
	return t
}

// send makes one attempt. The whole exchange, reading the response included,
// is bounded by the sender's timeout.
func (s *sender) send(ctx context.Context, m message) result {
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()

	res := result{started: s.now()}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.url, bytes.NewReader(m.body))
	if err != nil {
		res.errText = clean("invalid request: "+err.Error(), maxErrorRunes)
		res.duration = s.now().Sub(res.started)
		return res
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", userAgent)
	// Stable across retries of the same event, so a receiver can drop
	// duplicates (Standard Webhooks "webhook-id").
	req.Header.Set("Webhook-Id", m.eventID.String())

	resp, err := s.client.Do(req)
	if err != nil {
		res.errText = describe(err, s.timeout)
		res.duration = s.now().Sub(res.started)
		return res
	}
	defer resp.Body.Close()

	// A read error only shortens the snippet: the status code alone decides
	// the outcome. A 2xx is a success even if the body broke off, because the
	// endpoint has already accepted the message.
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxDrainBytes))
	res.statusCode = resp.StatusCode
	res.snippet = clean(string(body), maxSnippetRunes)
	res.duration = s.now().Sub(res.started)

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		res.errText = fmt.Sprintf("endpoint responded with HTTP %d", resp.StatusCode)
		if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusServiceUnavailable {
			res.retryAfter = parseRetryAfter(resp.Header.Get("Retry-After"), s.now())
		}
	}
	return res
}

// describe turns a transport error into a short message for the delivery
// history. The endpoint URL is not repeated (it is already on the endpoint).
func describe(err error, timeout time.Duration) string {
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err
	}
	var ne net.Error
	switch {
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &ne) && ne.Timeout():
		return fmt.Sprintf("timeout: no complete response within %s", timeout)
	case errors.Is(err, context.Canceled):
		return "canceled: the worker stopped before the endpoint responded"
	}
	return clean(err.Error(), maxErrorRunes)
}

// clean makes arbitrary bytes safe to store in a Postgres text column, which
// rejects invalid UTF-8 and NUL bytes, and cuts them to maxRunes characters.
// Without it, one endpoint answering with binary data would make every
// attempt to record its result fail.
func clean(s string, maxRunes int) string {
	s = strings.ToValidUTF8(s, "�")
	s = strings.ReplaceAll(s, "\x00", "")
	if utf8.RuneCountInString(s) <= maxRunes {
		return s
	}
	n := 0
	for i := range s {
		if n == maxRunes {
			return s[:i]
		}
		n++
	}
	return s
}
