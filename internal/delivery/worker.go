package delivery

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/burakcelik1061-star/hook-mini/internal/safedial"
	"github.com/burakcelik1061-star/hook-mini/internal/secretbox"
	"github.com/burakcelik1061-star/hook-mini/internal/store"
)

const (
	// recordTimeout bounds writing an attempt's outcome. It is independent of
	// shutdown, so a result that arrives while the worker is stopping is
	// still saved.
	recordTimeout = 10 * time.Second
	// maxErrorBackoff caps the pause after repeated database errors.
	maxErrorBackoff = 10 * time.Second
	// reapBatch is how many expired leases one reaper pass reclaims at most.
	reapBatch = 100

	lostLeaseReason        = "lease expired: the worker stopped before reporting a result"
	disabledEndpointReason = "endpoint is disabled; delivery abandoned without an attempt"
)

// errLeaseLost means the delivery is no longer leased to this attempt: the
// lease expired and was reclaimed, or the delivery was deleted.
var errLeaseLost = errors.New("lease lost")

// Config tunes a Worker. DefaultConfig gives production values.
type Config struct {
	// ID identifies the worker in leases and logs. Empty means
	// "<hostname>-<pid>-<random>".
	ID string
	// Concurrency is the maximum number of attempts in flight at once.
	Concurrency int
	// BatchSize is the maximum number of deliveries claimed per query.
	BatchSize int
	// PollInterval is how long to wait for new work once the queue has no
	// more due deliveries.
	PollInterval time.Duration
	// LeaseDuration is how long a claimed delivery stays reserved. It must
	// exceed RequestTimeout, or healthy attempts would be reclaimed.
	LeaseDuration time.Duration
	// ReapInterval is how often expired leases are reclaimed. Zero means
	// half the lease duration.
	ReapInterval time.Duration
	// RequestTimeout bounds one HTTP attempt, reading the response included.
	RequestTimeout time.Duration
	// DrainTimeout is how long Run waits for attempts in flight after its
	// context is cancelled before cancelling them too.
	DrainTimeout time.Duration
	// Retry decides when to retry and when to give up.
	Retry RetryPolicy
	// MaxPerEndpoint is the most attempts this worker has in flight to any
	// one endpoint; zero means no limit. It keeps a slow endpoint from taking
	// every slot and holding up deliveries to all the others.
	MaxPerEndpoint int
	// AutoDisableAfter disables an endpoint once this many consecutive
	// attempts to it have failed (any success resets the count). Zero turns
	// the feature off.
	AutoDisableAfter int
	// AllowPrivateNetworks lets requests reach loopback, private and
	// link-local addresses. Off by default; only for development and tests.
	AllowPrivateNetworks bool
	// Transport sends the HTTP requests. Nil means a transport that enforces
	// the destination policy at dial time; set it only in tests.
	Transport http.RoundTripper
	// Keys open the endpoint signing secrets, which the database stores
	// encrypted. Required.
	Keys *secretbox.Keyring
}

// DefaultConfig returns production defaults.
func DefaultConfig() Config {
	return Config{
		Concurrency:    10,
		BatchSize:      10,
		PollInterval:   500 * time.Millisecond,
		LeaseDuration:  60 * time.Second,
		RequestTimeout: 15 * time.Second,
		DrainTimeout:   20 * time.Second,
		MaxPerEndpoint: 3,
		Retry:          DefaultRetryPolicy(),
	}
}

func (c Config) validate() error {
	switch {
	case c.Concurrency < 1:
		return errors.New("concurrency must be at least 1")
	case c.BatchSize < 1:
		return errors.New("batch size must be at least 1")
	case c.PollInterval <= 0:
		return errors.New("poll interval must be positive")
	case c.RequestTimeout <= 0:
		return errors.New("request timeout must be positive")
	case c.LeaseDuration <= c.RequestTimeout:
		return errors.New("lease duration must be longer than the request timeout")
	case c.ReapInterval < 0:
		return errors.New("reap interval must not be negative")
	case c.MaxPerEndpoint < 0:
		return errors.New("per-endpoint maximum must not be negative")
	case c.AutoDisableAfter < 0:
		return errors.New("auto-disable threshold must not be negative")
	case c.DrainTimeout <= 0:
		return errors.New("drain timeout must be positive")
	case c.Keys == nil:
		return errors.New("encryption keys for the endpoint secrets are required")
	}
	if err := c.Retry.Validate(); err != nil {
		return fmt.Errorf("retry policy: %w", err)
	}
	return nil
}

// Worker delivers queued webhooks. Any number of workers may run against the
// same database; they coordinate only through row locks and leases.
type Worker struct {
	pool *pgxpool.Pool
	q    *store.Queries
	log  *slog.Logger
	cfg  Config
	http *sender

	// perEndpoint counts this worker's attempts in flight by endpoint.
	mu          sync.Mutex
	perEndpoint map[uuid.UUID]int
	// finishedOne is signalled (without blocking) whenever an attempt ends.
	finishedOne chan struct{}
}

// NewWorker validates cfg and returns a Worker.
func NewWorker(pool *pgxpool.Pool, log *slog.Logger, cfg Config) (*Worker, error) {
	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("invalid worker config: %w", err)
	}
	if cfg.ID == "" {
		cfg.ID = defaultID()
	}
	if cfg.ReapInterval == 0 {
		cfg.ReapInterval = cfg.LeaseDuration / 2
	}
	// Claiming more than can run at once would only hold leases idle.
	cfg.BatchSize = min(cfg.BatchSize, cfg.Concurrency)

	transport := cfg.Transport
	if transport == nil {
		transport = safedial.Policy{AllowPrivate: cfg.AllowPrivateNetworks}.Transport()
	}

	return &Worker{
		pool: pool,
		q:    store.New(pool),
		log:  log.With("worker_id", cfg.ID),
		cfg:  cfg,
		http: newSender(transport, cfg.RequestTimeout),

		perEndpoint: map[uuid.UUID]int{},
		finishedOne: make(chan struct{}, 1),
	}, nil
}

// ID returns the identity this worker writes into its leases.
func (w *Worker) ID() string { return w.cfg.ID }

// Run delivers webhooks until ctx is cancelled, then stops claiming new work,
// lets attempts in flight finish (for at most DrainTimeout) and returns.
func (w *Worker) Run(ctx context.Context) error {
	// Attempts run under their own context so that stopping the worker does
	// not abort requests mid-flight; it is cancelled only if draining takes
	// longer than DrainTimeout.
	attemptCtx, abortAttempts := context.WithCancel(context.Background())
	defer abortAttempts()

	reaperDone := make(chan struct{})
	go func() {
		defer close(reaperDone)
		w.reapLoop(ctx)
	}()

	var inFlight sync.WaitGroup
	slots := make(chan struct{}, w.cfg.Concurrency)
	errDelay := w.cfg.PollInterval

	for ctx.Err() == nil {
		// Block until at least one slot is free, then take as many more as
		// are free right now, up to the batch size.
		select {
		case slots <- struct{}{}:
		case <-ctx.Done():
			continue
		}
		n := 1
	fill:
		for n < w.cfg.BatchSize {
			select {
			case slots <- struct{}{}:
				n++
			default:
				break fill
			}
		}

		jobs, err := w.claim(ctx, n)
		for i := len(jobs); i < n; i++ {
			<-slots // give back the slots that got no work
		}
		if len(jobs) > n {
			// The claim query must never return more than n rows (see
			// ClaimDueDeliveries). If it ever did, wait for slots for the
			// extras rather than exceed the concurrency limit.
			w.log.Error("claim returned more deliveries than requested", "requested", n, "got", len(jobs))
			for i := n; i < len(jobs); i++ {
				slots <- struct{}{}
			}
		}
		if err != nil {
			w.log.Error("claiming deliveries failed", "error", err, "retry_in", errDelay)
			sleep(ctx, errDelay)
			errDelay = min(errDelay*2, maxErrorBackoff)
			continue
		}
		errDelay = w.cfg.PollInterval

		for _, job := range jobs {
			inFlight.Add(1)
			w.started(job.EndpointID)
			go func() {
				defer inFlight.Done()
				defer func() { <-slots }()
				defer w.finished(job.EndpointID)
				w.process(attemptCtx, job)
			}()
		}
		if len(jobs) < n {
			if w.anyBusy() {
				// Short because endpoints are at their per-endpoint maximum,
				// not because the queue is empty: there may well be due work
				// for them. Look again as soon as an attempt finishes (which
				// frees its endpoint), not a whole poll interval later.
				w.waitForFinish(ctx, w.cfg.PollInterval)
			} else {
				// The queue has no more due work right now.
				sleep(ctx, w.cfg.PollInterval)
			}
		}
	}

	<-reaperDone
	return w.drain(&inFlight, abortAttempts)
}

func (w *Worker) drain(inFlight *sync.WaitGroup, abort context.CancelFunc) error {
	done := make(chan struct{})
	go func() {
		inFlight.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-time.After(w.cfg.DrainTimeout):
		w.log.Warn("attempts still running after the drain timeout; cancelling them", "drain_timeout", w.cfg.DrainTimeout)
		abort()
		<-done
		return nil
	}
}

// claim leases up to n due deliveries. It runs under its own context: once a
// claim has been sent, its rows must be processed even if shutdown begins
// meanwhile, or their leases would expire and cost each delivery an attempt.
func (w *Worker) claim(ctx context.Context, n int) ([]store.ClaimDueDeliveriesRow, error) {
	if ctx.Err() != nil {
		return nil, nil
	}
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), recordTimeout)
	defer cancel()
	busy, perEndpoint := w.endpointRoom(n)
	return w.q.ClaimDueDeliveries(cctx, store.ClaimDueDeliveriesParams{
		LeaseMs:       w.cfg.LeaseDuration.Milliseconds(),
		WorkerID:      w.cfg.ID,
		BusyEndpoints: busy,
		// A few candidates per slot, so a crowded endpoint at the head of the
		// queue (more due than PerEndpoint) does not hide the work behind
		// it. Busy endpoints are filtered out before this limit applies.
		// Every candidate is row-locked for the statement, and a row lock is
		// written to the WAL, so this is kept small.
		ScanSize:    int32(min(max(n*claimScanFactor, 32), 1000)),
		PerEndpoint: int64(perEndpoint),
		BatchSize:   int32(n),
	})
}

// claimScanFactor is how many candidates the claim looks at per slot to fill.
const claimScanFactor = 4

// endpointRoom returns the endpoints this worker must not claim for (they
// have MaxPerEndpoint attempts in flight) and the most one claim may take for
// any single endpoint. A claim can still bring an endpoint up to almost twice
// the limit (in flight below it, then a full share in one claim); the bound
// is then 2*MaxPerEndpoint-1, never the whole batch.
func (w *Worker) endpointRoom(n int) (busy []uuid.UUID, perEndpoint int) {
	if w.cfg.MaxPerEndpoint == 0 {
		return []uuid.UUID{}, n
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	busy = []uuid.UUID{}
	for id, inFlight := range w.perEndpoint {
		if inFlight >= w.cfg.MaxPerEndpoint {
			busy = append(busy, id)
		}
	}
	return busy, w.cfg.MaxPerEndpoint
}

func (w *Worker) started(endpointID uuid.UUID) {
	w.mu.Lock()
	w.perEndpoint[endpointID]++
	w.mu.Unlock()
}

func (w *Worker) finished(endpointID uuid.UUID) {
	w.mu.Lock()
	if w.perEndpoint[endpointID]--; w.perEndpoint[endpointID] <= 0 {
		delete(w.perEndpoint, endpointID) // the map holds only endpoints in flight
	}
	w.mu.Unlock()
	select {
	case w.finishedOne <- struct{}{}:
	default: // a wake-up is already pending
	}
}

// anyBusy reports whether some endpoint has the per-endpoint maximum in
// flight, which is what can make a claim come back short with work left.
func (w *Worker) anyBusy() bool {
	if w.cfg.MaxPerEndpoint == 0 {
		return false
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, n := range w.perEndpoint {
		if n >= w.cfg.MaxPerEndpoint {
			return true
		}
	}
	return false
}

// waitForFinish returns when an attempt finishes, after at most d, or when
// ctx ends.
func (w *Worker) waitForFinish(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-w.finishedOne:
	case <-t.C:
	case <-ctx.Done():
	}
}

// process makes one attempt for a claimed delivery and records the outcome.
func (w *Worker) process(ctx context.Context, job store.ClaimDueDeliveriesRow) {
	log := w.log.With("delivery_id", job.ID, "endpoint_id", job.EndpointID, "event_id", job.EventID, "attempt", job.AttemptCount+1)

	if job.EndpointStatus != "active" {
		w.abandon(log, job, disabledEndpointReason)
		return
	}

	secrets, err := w.openSecrets(job)
	if err != nil {
		// The startup check guarantees every key is configured, so this is a
		// damaged or tampered value; the error names neither key nor secret.
		w.abandon(log, job, "the endpoint's signing secret cannot be decrypted: "+err.Error())
		return
	}
	msg, err := newMessage(job.EndpointUrl, job.EventID, job.EventType, job.EventCreatedAt, job.EventPayload, secrets...)
	if err != nil {
		// Only a corrupt secret or payload gets here, and retrying will not
		// fix either. The error never contains the secret itself.
		w.abandon(log, job, "could not build the request: "+err.Error())
		return
	}
	w.record(log, job, w.http.send(ctx, msg))
}

// openSecrets decrypts the secrets to sign with: the current one, and the
// previous one while its grace period lasts.
func (w *Worker) openSecrets(job store.ClaimDueDeliveriesRow) ([]string, error) {
	ctx := secretbox.EndpointContext(job.EndpointID)
	current, err := w.cfg.Keys.Open(job.EndpointSecret, ctx)
	if err != nil {
		return nil, err
	}
	secrets := []string{current}
	if job.PreviousSecretValid && job.EndpointPreviousSecret != nil {
		previous, err := w.cfg.Keys.Open(*job.EndpointPreviousSecret, ctx)
		if err != nil {
			return nil, err
		}
		secrets = append(secrets, previous)
	}
	return secrets, nil
}

// record writes the outcome of an attempt: the delivery's new state, the
// attempt row and the endpoint's failure counter, all in one transaction and
// all fenced by the lease token.
func (w *Worker) record(log *slog.Logger, job store.ClaimDueDeliveriesRow, res result) {
	if res.interrupted {
		w.recordInterrupted(log, job, res)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), recordTimeout)
	defer cancel()

	attempt := job.AttemptCount + 1
	// The retry budget is counted per round: a replayed delivery starts a new
	// round (round_base), while attempt numbers keep counting up.
	inRound := int(attempt - job.RoundBase)
	code := statusCode(res.statusCode)
	var (
		outcome         string
		delay           time.Duration
		disabledByLimit bool
	)

	err := pgx.BeginFunc(ctx, w.pool, func(tx pgx.Tx) error {
		q := w.q.WithTx(tx)

		// The endpoint row is locked before the delivery row. Deleting an
		// endpoint locks them in that same order (the endpoint, then its
		// deliveries through ON DELETE CASCADE); taking them the other way
		// round here could deadlock with a concurrent delete. If the lease
		// turns out to be lost, the whole transaction rolls back, counter
		// included.
		var err error
		if res.ok() {
			err = q.RecordEndpointSuccess(ctx, job.EndpointID)
		} else {
			var failures int32
			failures, err = q.RecordEndpointFailure(ctx, job.EndpointID)
			if errors.Is(err, pgx.ErrNoRows) {
				// The endpoint no longer exists: it was deleted while this
				// attempt was in flight, and its deliveries went with it.
				return errLeaseLost
			}
			if err == nil && w.cfg.AutoDisableAfter > 0 && int(failures) >= w.cfg.AutoDisableAfter {
				// Too many failures in a row: stop sending to this endpoint
				// until its owner enables it again. Idempotent: only an
				// active endpoint changes, so the first reason is kept.
				var n int64
				n, err = q.DisableEndpoint(ctx, store.DisableEndpointParams{
					ID:             job.EndpointID,
					DisabledReason: ptr(fmt.Sprintf("disabled automatically after %d consecutive failed attempts", failures)),
				})
				disabledByLimit = n > 0
			}
		}
		if err != nil {
			return err
		}

		var n int64
		switch {
		case res.ok():
			outcome = "succeeded"
			n, err = q.MarkDeliverySucceeded(ctx, store.MarkDeliverySucceededParams{
				ID: job.ID, LeaseToken: job.LeaseToken, StatusCode: int32(res.statusCode),
			})
		case inRound >= w.cfg.Retry.MaxAttempts:
			outcome = "dead"
			n, err = q.MarkDeliveryDead(ctx, store.MarkDeliveryDeadParams{
				ID: job.ID, LeaseToken: job.LeaseToken, AttemptsUsed: 1, StatusCode: code, LastError: res.errText,
			})
		default:
			outcome = "retry"
			delay = w.cfg.Retry.nextDelay(inRound, res.retryAfter)
			n, err = q.ScheduleDeliveryRetry(ctx, store.ScheduleDeliveryRetryParams{
				ID: job.ID, LeaseToken: job.LeaseToken, DelayMs: delay.Milliseconds(), StatusCode: code, LastError: res.errText,
			})
		}
		if err != nil {
			return err
		}
		if n == 0 {
			return errLeaseLost
		}

		_, err = q.CreateDeliveryAttempt(ctx, store.CreateDeliveryAttemptParams{
			DeliveryID:      job.ID,
			AttemptNo:       attempt,
			StartedAt:       res.started,
			DurationMs:      int32(min(res.duration.Milliseconds(), math.MaxInt32)),
			StatusCode:      code,
			ResponseSnippet: nonEmpty(res.snippet),
			Error:           nonEmpty(res.errText),
		})
		return err
	})

	if err == nil && disabledByLimit {
		log.Warn("endpoint disabled automatically: too many consecutive failed attempts",
			"limit", w.cfg.AutoDisableAfter)
	}
	attrs := []any{"status_code", res.statusCode, "duration_ms", res.duration.Milliseconds()}
	if res.detail != "" {
		attrs = append(attrs, "detail", res.detail)
	}
	switch {
	case errors.Is(err, errLeaseLost):
		log.Warn("result discarded: the lease was lost (expired and reclaimed, or the delivery was deleted)", attrs...)
	case err != nil:
		// The delivery stays in flight; its lease will expire and the reaper
		// returns it to the queue, so the attempt is retried, not lost.
		log.Error("recording the attempt failed", append(attrs, "error", err)...)
	case outcome == "succeeded":
		log.Info("delivered", attrs...)
	case outcome == "retry":
		log.Warn("attempt failed; will retry", append(attrs, "error", res.errText, "retry_in", delay.Round(time.Millisecond))...)
	default:
		log.Warn("attempt failed; giving up after the last attempt", append(attrs, "error", res.errText)...)
	}
}

// recordInterrupted writes an attempt the worker cancelled itself because
// draining took too long. The endpoint did nothing wrong, so its failure
// counter is left alone and the attempt does not use up the delivery's retry
// budget; otherwise every slow shutdown could kill deliveries that were on
// their last attempt and push healthy endpoints towards being disabled.
//
// Only the delivery row is locked here (the attempt row's foreign key points
// at that same row), so this cannot deadlock with an endpoint being deleted,
// which locks the endpoint first and then its deliveries.
func (w *Worker) recordInterrupted(log *slog.Logger, job store.ClaimDueDeliveriesRow, res result) {
	ctx, cancel := context.WithTimeout(context.Background(), recordTimeout)
	defer cancel()

	err := pgx.BeginFunc(ctx, w.pool, func(tx pgx.Tx) error {
		q := w.q.WithTx(tx)
		n, err := q.ReturnInterruptedDelivery(ctx, store.ReturnInterruptedDeliveryParams{
			ID: job.ID, LeaseToken: job.LeaseToken, LastError: res.errText,
		})
		if err != nil {
			return err
		}
		if n == 0 {
			return errLeaseLost
		}
		_, err = q.CreateDeliveryAttempt(ctx, store.CreateDeliveryAttemptParams{
			DeliveryID: job.ID,
			AttemptNo:  job.AttemptCount + 1,
			StartedAt:  res.started,
			DurationMs: int32(min(res.duration.Milliseconds(), math.MaxInt32)),
			Error:      nonEmpty(res.errText),
		})
		return err
	})
	switch {
	case errors.Is(err, errLeaseLost):
		log.Warn("interrupted attempt discarded: the lease was lost")
	case err != nil:
		log.Error("recording the interrupted attempt failed", "error", err)
	default:
		log.Warn("attempt interrupted by shutdown; the delivery is queued again without using up a retry",
			"duration_ms", res.duration.Milliseconds())
	}
}

// abandon marks a delivery dead without making an attempt.
func (w *Worker) abandon(log *slog.Logger, job store.ClaimDueDeliveriesRow, reason string) {
	ctx, cancel := context.WithTimeout(context.Background(), recordTimeout)
	defer cancel()
	n, err := w.q.MarkDeliveryDead(ctx, store.MarkDeliveryDeadParams{
		ID: job.ID, LeaseToken: job.LeaseToken, AttemptsUsed: 0, LastError: reason,
	})
	switch {
	case err != nil:
		log.Error("abandoning the delivery failed", "reason", reason, "error", err)
	case n == 0:
		log.Warn("delivery not abandoned: the lease was lost", "reason", reason)
	default:
		log.Warn("delivery abandoned", "reason", reason)
	}
}

func (w *Worker) reapLoop(ctx context.Context) {
	t := time.NewTicker(w.cfg.ReapInterval)
	defer t.Stop()
	for {
		// Reap right away on start: leases left by a previous instance of
		// this worker may already have expired.
		for {
			n, err := w.reapOnce(ctx)
			if err != nil {
				if ctx.Err() == nil {
					w.log.Error("reclaiming expired leases failed", "error", err)
				}
				break
			}
			if n < reapBatch {
				break
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// reapOnce returns up to reapBatch deliveries with expired leases to the
// queue and records the lost attempts, in one transaction.
func (w *Worker) reapOnce(ctx context.Context) (int, error) {
	var rows []store.ReclaimExpiredLeasesRow
	err := pgx.BeginFunc(ctx, w.pool, func(tx pgx.Tx) error {
		q := w.q.WithTx(tx)
		var err error
		rows, err = q.ReclaimExpiredLeases(ctx, store.ReclaimExpiredLeasesParams{
			MaxAttempts: int32(w.cfg.Retry.MaxAttempts), Reason: lostLeaseReason, BatchSize: reapBatch,
		})
		if err != nil || len(rows) == 0 {
			return err
		}
		ids := make([]uuid.UUID, len(rows))
		nos := make([]int32, len(rows))
		for i, r := range rows {
			ids[i], nos[i] = r.ID, r.AttemptCount
		}
		return q.RecordLostAttempts(ctx, store.RecordLostAttemptsParams{DeliveryIds: ids, AttemptNos: nos, Reason: lostLeaseReason})
	})
	if err != nil {
		return 0, err
	}
	for _, r := range rows {
		prev := ""
		if r.PreviousWorker != nil {
			prev = *r.PreviousWorker
		}
		w.log.Warn("reclaimed a delivery whose lease expired",
			"delivery_id", r.ID, "previous_worker", prev, "attempts", r.AttemptCount, "status", r.Status)
	}
	return len(rows), nil
}

func ptr[T any](v T) *T { return &v }

func sleep(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

func statusCode(c int) *int32 {
	if c == 0 {
		return nil
	}
	v := int32(c)
	return &v
}

func nonEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func defaultID() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "worker"
	}
	b := make([]byte, 3)
	_, _ = rand.Read(b)
	return fmt.Sprintf("%s-%d-%x", host, os.Getpid(), b)
}
