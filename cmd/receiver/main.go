// Command receiver is a development tool: a webhook endpoint that logs every
// request it gets. It is not part of the service. Use it to watch deliveries,
// retries and signatures locally:
//
//	POST /anything        answers 200
//	POST /status/503      answers 503 (any code from 100 to 599)
//	POST /delay/3000      waits 3000 ms (at most 60000), then answers 200
//
// It listens on RECEIVER_ADDR (default :9000). If RECEIVER_SECRETS holds one
// or more endpoint secrets (separated by spaces or commas), every request's
// Standard Webhooks signature is verified with pkg/webhook, exactly as a real
// receiver would, and a request that fails is answered 401.
package main

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/burakcelik1061-star/hook-mini/pkg/webhook"
)

func main() {
	addr := os.Getenv("RECEIVER_ADDR")
	if addr == "" {
		addr = ":9000"
	}
	log := slog.New(slog.NewTextHandler(os.Stdout, nil)).With("service", "receiver")

	var verifier *webhook.Verifier
	if secrets := strings.FieldsFunc(os.Getenv("RECEIVER_SECRETS"), func(r rune) bool { return r == ',' || r == ' ' }); len(secrets) > 0 {
		v, err := webhook.NewVerifier(secrets...)
		if err != nil {
			fmt.Fprintln(os.Stderr, "receiver: RECEIVER_SECRETS:", err)
			os.Exit(1)
		}
		verifier = v
	}

	srv := &http.Server{
		Addr:              addr,
		Handler:           handler(log, verifier),
		ReadHeaderTimeout: 5 * time.Second,
	}
	log.Info("receiver listening", "addr", addr, "verifies_signatures", verifier != nil)
	if err := srv.ListenAndServe(); err != nil {
		fmt.Fprintln(os.Stderr, "receiver:", err)
		os.Exit(1)
	}
}

func handler(log *slog.Logger, verifier *webhook.Verifier) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))

		signature := "unchecked"
		if verifier != nil {
			if err := verifier.Verify(r.Header, body); err != nil {
				log.Warn("webhook rejected", "path", r.URL.Path, "webhook_id", r.Header.Get(webhook.HeaderID), "reason", err)
				http.Error(w, "invalid signature", http.StatusUnauthorized)
				return
			}
			signature = "valid"
		}

		status := http.StatusOK
		if code, ok := strings.CutPrefix(r.URL.Path, "/status/"); ok {
			if n, err := strconv.Atoi(code); err == nil && n >= 100 && n <= 599 {
				status = n
			}
		}

		if ms, ok := strings.CutPrefix(r.URL.Path, "/delay/"); ok {
			if n, err := strconv.Atoi(ms); err == nil && n > 0 {
				select {
				case <-time.After(time.Duration(min(n, 60_000)) * time.Millisecond):
				case <-r.Context().Done():
					return
				}
			}
		}

		log.Info("webhook received",
			"method", r.Method, "path", r.URL.Path, "answer", status, "signature", signature,
			"signatures_sent", len(strings.Fields(r.Header.Get(webhook.HeaderSignature))),
			"webhook_id", r.Header.Get(webhook.HeaderID), "user_agent", r.Header.Get("User-Agent"),
			"body", truncate(string(body), 500))

		w.WriteHeader(status)
		fmt.Fprintf(w, "receiver answered %d\n", status)
	})
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
