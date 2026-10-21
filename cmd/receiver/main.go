// Command receiver is a development tool: a webhook endpoint that logs every
// request it gets. It is not part of the service. Use it to watch deliveries
// and retries locally:
//
//	POST /anything        answers 200
//	POST /status/503      answers 503 (any code from 100 to 599)
//
// It listens on RECEIVER_ADDR (default :9000).
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
)

func main() {
	addr := os.Getenv("RECEIVER_ADDR")
	if addr == "" {
		addr = ":9000"
	}
	log := slog.New(slog.NewTextHandler(os.Stdout, nil)).With("service", "receiver")

	srv := &http.Server{
		Addr:              addr,
		Handler:           handler(log),
		ReadHeaderTimeout: 5 * time.Second,
	}
	log.Info("receiver listening", "addr", addr)
	if err := srv.ListenAndServe(); err != nil {
		fmt.Fprintln(os.Stderr, "receiver:", err)
		os.Exit(1)
	}
}

func handler(log *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))

		status := http.StatusOK
		if code, ok := strings.CutPrefix(r.URL.Path, "/status/"); ok {
			if n, err := strconv.Atoi(code); err == nil && n >= 100 && n <= 599 {
				status = n
			}
		}

		log.Info("webhook received",
			"method", r.Method, "path", r.URL.Path, "answer", status,
			"webhook_id", r.Header.Get("Webhook-Id"), "user_agent", r.Header.Get("User-Agent"),
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
