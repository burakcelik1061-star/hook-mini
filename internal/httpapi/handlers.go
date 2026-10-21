package httpapi

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/burakcelik1061-star/hook-mini/internal/service"
	"github.com/burakcelik1061-star/hook-mini/internal/store"
)

type handlers struct {
	svc *service.Service
	log *slog.Logger
}

// Response types. Responses are built from these explicit structs, never from
// the store's row types: a new database column must be exposed on purpose,
// not by accident.

type endpointResponse struct {
	ID                  uuid.UUID  `json:"id"`
	URL                 string     `json:"url"`
	Description         string     `json:"description"`
	Status              string     `json:"status"`
	ConsecutiveFailures int32      `json:"consecutive_failures"`
	DisabledAt          *time.Time `json:"disabled_at,omitempty"`
	DisabledReason      *string    `json:"disabled_reason,omitempty"`
	CreatedAt           time.Time  `json:"created_at"`
	UpdatedAt           time.Time  `json:"updated_at"`
	// PreviousSecretExpiresAt is set while a rotated-out secret still signs
	// requests. The previous secret itself is never returned.
	PreviousSecretExpiresAt *time.Time `json:"previous_secret_expires_at,omitempty"`
	// Secret is present only in the responses to the requests that create
	// the endpoint or rotate its secret. It cannot be read again.
	Secret string `json:"secret,omitempty"`
}

func newEndpointResponse(e store.Endpoint, withSecret bool) endpointResponse {
	r := endpointResponse{
		ID: e.ID, URL: e.Url, Description: e.Description, Status: e.Status,
		ConsecutiveFailures: e.ConsecutiveFailures, DisabledAt: e.DisabledAt, DisabledReason: e.DisabledReason,
		CreatedAt: e.CreatedAt, UpdatedAt: e.UpdatedAt,
	}
	if e.PreviousSecretExpiresAt != nil && e.PreviousSecretExpiresAt.After(time.Now()) {
		r.PreviousSecretExpiresAt = e.PreviousSecretExpiresAt
	}
	if withSecret {
		r.Secret = e.Secret
	}
	return r
}

type eventResponse struct {
	ID             uuid.UUID       `json:"id"`
	Type           string          `json:"type"`
	Payload        json.RawMessage `json:"payload"`
	IdempotencyKey *string         `json:"idempotency_key,omitempty"`
	CreatedAt      time.Time       `json:"created_at"`
	DeliveryCount  *int            `json:"delivery_count,omitempty"`
}

func newEventResponse(e store.Event, deliveries *int) eventResponse {
	return eventResponse{
		ID: e.ID, Type: e.Type, Payload: e.Payload, IdempotencyKey: e.IdempotencyKey,
		CreatedAt: e.CreatedAt, DeliveryCount: deliveries,
	}
}

type listResponse[T any] struct {
	Data []T `json:"data"`
	// NextCursor is null on the last page.
	NextCursor *string `json:"next_cursor"`
}

func newListResponse[T any](items []T, next string) listResponse[T] {
	out := listResponse[T]{Data: items}
	if out.Data == nil {
		out.Data = []T{}
	}
	if next != "" {
		out.NextCursor = &next
	}
	return out
}

// pathID parses the {id} route parameter. A malformed id is reported as "not
// found", the same as an id that does not exist.
func pathID(r *http.Request) (uuid.UUID, error) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		return uuid.Nil, service.ErrNotFound
	}
	return id, nil
}

func pageFrom(r *http.Request) (service.Page, error) {
	var p service.Page
	if s := r.URL.Query().Get("limit"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil {
			return p, badRequest("limit", "must be an integer")
		}
		if n < 1 || n > service.MaxPageSize {
			return p, badRequest("limit", "must be between 1 and "+strconv.Itoa(service.MaxPageSize))
		}
		p.Limit = n
	}
	p.Cursor = r.URL.Query().Get("cursor")
	return p, nil
}

func (h *handlers) fail(w http.ResponseWriter, r *http.Request, err error) {
	writeError(w, r, h.log, err)
}

// ---- endpoints ----

func (h *handlers) createEndpoint(w http.ResponseWriter, r *http.Request) {
	var in struct {
		URL         string `json:"url"`
		Description string `json:"description"`
	}
	if err := decodeJSON(w, r, &in, maxEndpointBody); err != nil {
		h.fail(w, r, err)
		return
	}
	ep, err := h.svc.CreateEndpoint(r.Context(), principalFrom(r.Context()).TenantID,
		service.EndpointInput{URL: in.URL, Description: in.Description})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	w.Header().Set("Location", "/v1/endpoints/"+ep.ID.String())
	writeJSON(w, http.StatusCreated, newEndpointResponse(ep, true))
}

func (h *handlers) listEndpoints(w http.ResponseWriter, r *http.Request) {
	page, err := pageFrom(r)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	res, err := h.svc.ListEndpoints(r.Context(), principalFrom(r.Context()).TenantID, page)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	items := make([]endpointResponse, 0, len(res.Items))
	for _, e := range res.Items {
		items = append(items, newEndpointResponse(e, false))
	}
	writeJSON(w, http.StatusOK, newListResponse(items, res.NextCursor))
}

func (h *handlers) getEndpoint(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	ep, err := h.svc.GetEndpoint(r.Context(), principalFrom(r.Context()).TenantID, id)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, newEndpointResponse(ep, false))
}

func (h *handlers) updateEndpoint(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	var in struct {
		URL         *string `json:"url"`
		Description *string `json:"description"`
	}
	if err := decodeJSON(w, r, &in, maxEndpointBody); err != nil {
		h.fail(w, r, err)
		return
	}
	ep, err := h.svc.UpdateEndpoint(r.Context(), principalFrom(r.Context()).TenantID, id,
		service.EndpointPatch{URL: in.URL, Description: in.Description})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, newEndpointResponse(ep, false))
}

// rotateSecret replaces an endpoint's signing secret. It takes no body.
func (h *handlers) rotateSecret(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	ep, err := h.svc.RotateEndpointSecret(r.Context(), principalFrom(r.Context()).TenantID, id)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, newEndpointResponse(ep, true))
}

func (h *handlers) deleteEndpoint(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	if err := h.svc.DeleteEndpoint(r.Context(), principalFrom(r.Context()).TenantID, id); err != nil {
		h.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---- events ----

func (h *handlers) createEvent(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Type    string          `json:"type"`
		Payload json.RawMessage `json:"payload"`
	}
	if err := decodeJSON(w, r, &in, maxEventBody); err != nil {
		h.fail(w, r, err)
		return
	}

	// Header.Values distinguishes "absent" from "present but empty".
	var key *string
	switch vals := r.Header.Values("Idempotency-Key"); len(vals) {
	case 0:
	case 1:
		key = &vals[0]
	default:
		h.fail(w, r, badRequest("Idempotency-Key", "must be sent at most once"))
		return
	}

	res, err := h.svc.IngestEvent(r.Context(), principalFrom(r.Context()).TenantID, service.IngestInput{
		Type: in.Type, Payload: in.Payload, IdempotencyKey: key,
	})
	if err != nil {
		h.fail(w, r, err)
		return
	}

	w.Header().Set("Location", "/v1/events/"+res.Event.ID.String())
	status := http.StatusCreated
	if res.Replayed {
		// Same key, same body: the original result again, nothing created.
		w.Header().Set("Idempotent-Replayed", "true")
		status = http.StatusOK
	}
	writeJSON(w, status, newEventResponse(res.Event, &res.DeliveryCount))
}

func (h *handlers) listEvents(w http.ResponseWriter, r *http.Request) {
	page, err := pageFrom(r)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	res, err := h.svc.ListEvents(r.Context(), principalFrom(r.Context()).TenantID, page)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	items := make([]eventResponse, 0, len(res.Items))
	for _, e := range res.Items {
		items = append(items, newEventResponse(e, nil))
	}
	writeJSON(w, http.StatusOK, newListResponse(items, res.NextCursor))
}

func (h *handlers) getEvent(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	res, err := h.svc.GetEvent(r.Context(), principalFrom(r.Context()).TenantID, id)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, newEventResponse(res.Event, &res.DeliveryCount))
}
