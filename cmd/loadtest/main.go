// Command loadtest measures a running Hook Mini stack from the outside. It is
// a development tool, not part of the service.
//
// It registers endpoints that point at a sink it runs itself, submits events
// through the public API, and records when each delivery arrives at the sink.
// What it reports:
//
//	ingest     requests per second and latency of POST /v1/events
//	delivery   deliveries per second end to end, and submit-to-delivered latency
//
// Every run ends with a consistency check against the database: a result
// whose counts do not add up is reported as invalid, never as a number.
//
// The sink must be reachable from the workers (with docker compose that is
// host.docker.internal) and the workers must allow private destinations
// (ALLOW_PRIVATE_DESTINATIONS=true); see docs/11-yuk-testi.md.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/burakcelik1061-star/hook-mini/pkg/webhook"
)

type config struct {
	api, key, db         string
	sinkHost             string
	sinkPort             int
	scenario             string
	endpoints            int
	clients              int
	events               int
	rate                 float64
	slowPct, failPct     int
	slowDelay            time.Duration
	drainTimeout         time.Duration
	skipVerifySignatures bool
}

func main() {
	var c config
	flag.StringVar(&c.api, "api", "http://localhost:8080", "base URL of the api")
	flag.StringVar(&c.key, "key", os.Getenv("LOADTEST_KEY"), "API key (or LOADTEST_KEY)")
	flag.StringVar(&c.db, "db", os.Getenv("LOADTEST_DB"), "Postgres URL for the final consistency check (or LOADTEST_DB)")
	flag.StringVar(&c.sinkHost, "sink-host", "host.docker.internal", "host name the workers use to reach this process")
	flag.IntVar(&c.sinkPort, "sink-port", 9100, "port of the sink")
	flag.StringVar(&c.scenario, "scenario", "delivery", "ingest | delivery | mixed")
	flag.IntVar(&c.endpoints, "endpoints", 5, "endpoints registered (fan-out per event)")
	flag.IntVar(&c.clients, "clients", 32, "concurrent submitting clients")
	flag.IntVar(&c.events, "events", 2000, "events to submit")
	flag.Float64Var(&c.rate, "rate", 0, "events per second to submit (0 = as fast as possible)")
	flag.IntVar(&c.slowPct, "slow-pct", 10, "mixed: percent of endpoints that answer slowly")
	flag.IntVar(&c.failPct, "fail-pct", 10, "mixed: percent of endpoints that answer 500")
	flag.DurationVar(&c.slowDelay, "slow-delay", 500*time.Millisecond, "mixed: how long a slow endpoint takes")
	flag.DurationVar(&c.drainTimeout, "drain-timeout", 2*time.Minute, "how long to wait for deliveries after the last event")
	flag.BoolVar(&c.skipVerifySignatures, "skip-verify", false, "do not verify signatures at the sink")
	flag.Parse()

	if err := run(c); err != nil {
		fmt.Fprintln(os.Stderr, "loadtest:", err)
		os.Exit(1)
	}
}

type endpoint struct {
	index  int
	id     uuid.UUID
	path   string
	secret string
	kind   string // ok | slow | fail
	ver    *webhook.Verifier
}

func run(c config) error {
	if c.key == "" {
		return errors.New("an API key is required (-key or LOADTEST_KEY)")
	}
	if c.scenario != "ingest" && c.scenario != "delivery" && c.scenario != "mixed" {
		return fmt.Errorf("unknown scenario %q", c.scenario)
	}
	ctx := context.Background()
	runID := uuid.NewString()[:8]
	client := &http.Client{
		Timeout:   30 * time.Second,
		Transport: &http.Transport{MaxIdleConns: c.clients * 2, MaxIdleConnsPerHost: c.clients * 2, IdleConnTimeout: 90 * time.Second},
	}

	// ---- endpoints ----
	eps := make([]*endpoint, c.endpoints)
	slow := c.endpoints * c.slowPct / 100
	fail := c.endpoints * c.failPct / 100
	for i := range eps {
		kind := "ok"
		if c.scenario == "mixed" {
			switch {
			case i < slow:
				kind = "slow"
			case i < slow+fail:
				kind = "fail"
			}
		}
		path := fmt.Sprintf("/%s/%d", runID, i)
		body, _ := json.Marshal(map[string]string{"url": fmt.Sprintf("http://%s:%d%s", c.sinkHost, c.sinkPort, path), "description": "loadtest " + runID})
		var out struct {
			ID     uuid.UUID `json:"id"`
			Secret string    `json:"secret"`
		}
		if err := do(ctx, client, c, "POST", "/v1/endpoints", body, &out); err != nil {
			return fmt.Errorf("register endpoint %d: %w", i, err)
		}
		v, err := webhook.NewVerifier(out.Secret)
		if err != nil {
			return err
		}
		eps[i] = &endpoint{index: i, id: out.ID, path: path, secret: out.Secret, kind: kind, ver: v}
	}
	byPath := map[string]*endpoint{}
	for _, e := range eps {
		byPath[e.path] = e
	}

	// ---- sink ----
	var (
		mu         sync.Mutex
		arrivals   = map[string]time.Time{} // "<event id>|<endpoint index>" -> first arrival
		duplicates atomic.Int64
		badSig     atomic.Int64
		unknown    atomic.Int64
	)
	sink := &http.Server{
		ReadHeaderTimeout: 5 * time.Second,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			now := time.Now()
			body, _ := io.ReadAll(r.Body)
			e := byPath[r.URL.Path]
			if e == nil {
				unknown.Add(1)
				http.Error(w, "unknown", http.StatusNotFound)
				return
			}
			if !c.skipVerifySignatures && e.ver.Verify(r.Header, body) != nil {
				badSig.Add(1)
			}
			switch e.kind {
			case "fail":
				w.WriteHeader(http.StatusInternalServerError)
				return
			case "slow":
				time.Sleep(c.slowDelay)
			}
			key := fmt.Sprintf("%s|%d", r.Header.Get(webhook.HeaderID), e.index)
			mu.Lock()
			if _, seen := arrivals[key]; seen {
				duplicates.Add(1)
			} else {
				arrivals[key] = now
			}
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
		}),
	}
	ln, err := net.Listen("tcp", fmt.Sprintf("0.0.0.0:%d", c.sinkPort))
	if err != nil {
		return fmt.Errorf("start the sink: %w", err)
	}
	go func() { _ = sink.Serve(ln) }()
	defer sink.Close()

	// ---- submit ----
	type sent struct {
		id uuid.UUID
		at time.Time // when the request started
	}
	var (
		sentMu    sync.Mutex
		sentList  = make([]sent, 0, c.events)
		ingestLat = make([]time.Duration, 0, c.events)
		failures  atomic.Int64
		next      atomic.Int64
	)
	var tick <-chan time.Time
	if c.rate > 0 {
		t := time.NewTicker(time.Duration(float64(time.Second) / c.rate))
		defer t.Stop()
		tick = t.C
	}
	fmt.Fprintf(os.Stderr, "run %s: %s, %d endpoints (%d slow, %d failing), %d events, %d clients\n", runID, c.scenario, c.endpoints, slow, fail, c.events, c.clients)
	start := time.Now()
	var wg sync.WaitGroup
	for range c.clients {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				n := int(next.Add(1))
				if n > c.events {
					return
				}
				if tick != nil {
					<-tick
				}
				body, _ := json.Marshal(map[string]any{"type": "load.test", "payload": map[string]any{"n": n, "run": runID, "pad": strings.Repeat("x", 200), "r": rand.IntN(1000)}})
				t0 := time.Now()
				var out struct {
					ID uuid.UUID `json:"id"`
				}
				if err := do(ctx, client, c, "POST", "/v1/events", body, &out); err != nil {
					failures.Add(1)
					continue
				}
				d := time.Since(t0)
				sentMu.Lock()
				sentList = append(sentList, sent{out.ID, t0})
				ingestLat = append(ingestLat, d)
				sentMu.Unlock()
			}
		}()
	}
	wg.Wait()
	ingestElapsed := time.Since(start)
	accepted := len(sentList)

	res := map[string]any{
		"scenario": c.scenario, "endpoints": c.endpoints, "events_requested": c.events, "events_accepted": accepted,
		"submit_failures": failures.Load(), "clients": c.clients, "rate_limit": c.rate,
		"ingest_seconds": round(ingestElapsed.Seconds()), "ingest_per_second": round(float64(accepted) / ingestElapsed.Seconds()),
		"ingest_ms": percentiles(ingestLat),
	}
	problems := []string{}
	if failures.Load() > 0 {
		problems = append(problems, fmt.Sprintf("%d submissions failed", failures.Load()))
	}
	healthy := 0
	for _, e := range eps {
		if e.kind != "fail" {
			healthy++
		}
	}

	// ---- delivery ----
	if c.scenario != "ingest" {
		want := accepted * healthy
		deadline := time.Now().Add(c.drainTimeout)
		for time.Now().Before(deadline) {
			mu.Lock()
			got := len(arrivals)
			mu.Unlock()
			if got >= want {
				break
			}
			time.Sleep(50 * time.Millisecond)
		}

		kinds := map[int]string{}
		for _, e := range eps {
			kinds[e.index] = e.kind
		}
		sentAt := map[uuid.UUID]time.Time{}
		for _, s := range sentList {
			sentAt[s.id] = s.at
		}
		mu.Lock()
		got := len(arrivals)
		var last time.Time
		lat := map[string][]time.Duration{}
		for k, at := range arrivals {
			if at.After(last) {
				last = at
			}
			id, idx, _ := strings.Cut(k, "|")
			eid, err := uuid.Parse(id)
			if err != nil {
				continue
			}
			var i int
			fmt.Sscanf(idx, "%d", &i)
			if t0, ok := sentAt[eid]; ok {
				lat[kinds[i]] = append(lat[kinds[i]], at.Sub(t0))
			}
		}
		mu.Unlock()

		total := last.Sub(start)
		res["deliveries_expected"] = want
		res["deliveries_received"] = got
		res["delivery_seconds"] = round(total.Seconds())
		res["deliveries_per_second"] = round(float64(got) / total.Seconds())
		res["submit_to_delivered_ms"] = percentiles(lat["ok"])
		if c.scenario == "mixed" {
			res["submit_to_delivered_slow_endpoints_ms"] = percentiles(lat["slow"])
		}
		res["duplicates"] = duplicates.Load()
		res["bad_signatures"] = badSig.Load()
		if got != want {
			problems = append(problems, fmt.Sprintf("received %d of %d expected deliveries", got, want))
		}
		if badSig.Load() > 0 {
			problems = append(problems, fmt.Sprintf("%d requests with an invalid signature", badSig.Load()))
		}
		if unknown.Load() > 0 {
			problems = append(problems, fmt.Sprintf("%d requests for unknown paths", unknown.Load()))
		}
	}

	// ---- consistency check against the database ----
	if c.db != "" {
		byEndpoint, err := dbCounts(ctx, c.db, eps)
		if err != nil {
			return fmt.Errorf("consistency check: %w", err)
		}
		totals := map[string]int{}
		healthyRows, healthySucceeded, failingRows := 0, 0, 0
		for _, e := range eps {
			for status, n := range byEndpoint[e.id] {
				totals[status] += n
				if e.kind == "fail" {
					failingRows += n
				} else {
					healthyRows += n
					if status == "succeeded" {
						healthySucceeded += n
					}
				}
			}
		}
		res["db_deliveries"] = totals
		// Every event creates a delivery for each endpoint that is active when
		// it arrives. Healthy endpoints stay active, so they must have exactly
		// one per event. An endpoint that keeps failing is disabled by the
		// service after a while (ENDPOINT_AUTO_DISABLE_AFTER), after which it
		// gets no deliveries: it may have fewer, never more.
		if healthyRows != accepted*healthy {
			problems = append(problems, fmt.Sprintf("the database has %d deliveries for the healthy endpoints, want %d (events x endpoints)", healthyRows, accepted*healthy))
		}
		if failingRows > accepted*(len(eps)-healthy) {
			problems = append(problems, fmt.Sprintf("the failing endpoints have %d deliveries, more than one per event", failingRows))
		}
		res["failing_endpoint_deliveries"] = failingRows
		if c.scenario != "ingest" && healthySucceeded != accepted*healthy {
			problems = append(problems, fmt.Sprintf("%d deliveries to healthy endpoints are succeeded in the database, want %d", healthySucceeded, accepted*healthy))
		}
	} else {
		problems = append(problems, "no -db given: the result is not checked against the database")
	}

	res["valid"] = len(problems) == 0
	res["problems"] = problems
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(res)
}

// dbCounts returns, per endpoint, how many deliveries are in each status.
func dbCounts(ctx context.Context, url string, eps []*endpoint) (map[uuid.UUID]map[string]int, error) {
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, err
	}
	defer pool.Close()
	ids := make([]uuid.UUID, len(eps))
	for i, e := range eps {
		ids[i] = e.id
	}
	rows, err := pool.Query(ctx, `SELECT endpoint_id, status, count(*) FROM deliveries WHERE endpoint_id = ANY($1) GROUP BY endpoint_id, status`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[uuid.UUID]map[string]int{}
	for rows.Next() {
		var id uuid.UUID
		var status string
		var n int
		if err := rows.Scan(&id, &status, &n); err != nil {
			return nil, err
		}
		if out[id] == nil {
			out[id] = map[string]int{}
		}
		out[id][status] = n
	}
	return out, rows.Err()
}

// do sends one API request and decodes a JSON answer into out.
func do(ctx context.Context, client *http.Client, c config, method, path string, body []byte, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, c.api+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("%s %s: %d %.120s", method, path, resp.StatusCode, data)
	}
	if out != nil {
		return json.Unmarshal(data, out)
	}
	return nil
}

func percentiles(d []time.Duration) map[string]float64 {
	if len(d) == 0 {
		return nil
	}
	s := append([]time.Duration(nil), d...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	at := func(p float64) float64 {
		return round(float64(s[min(len(s)-1, int(p*float64(len(s))))]) / float64(time.Millisecond))
	}
	return map[string]float64{"p50": at(0.50), "p95": at(0.95), "p99": at(0.99), "max": round(float64(s[len(s)-1]) / float64(time.Millisecond))}
}

func round(f float64) float64 { return float64(int(f*10+0.5)) / 10 }
