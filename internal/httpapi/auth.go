package httpapi

import (
	"context"
	"net/http"
	"strings"

	"github.com/burakcelik1061-star/hook-mini/internal/service"
)

type principalKey struct{}

// principalFrom returns the authenticated caller. It is only called by
// handlers mounted behind authenticate, so it is always set there.
func principalFrom(ctx context.Context) service.Principal {
	p, _ := ctx.Value(principalKey{}).(service.Principal)
	return p
}

// authenticate requires "Authorization: Bearer <api key>" and puts the
// caller's identity in the request context. All failures look identical to
// the client (see service.Authenticate).
func (h *handlers) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		scheme, token, found := strings.Cut(r.Header.Get("Authorization"), " ")
		if !found || !strings.EqualFold(scheme, "Bearer") || token == "" {
			writeError(w, r, h.log, service.ErrUnauthorized)
			return
		}
		p, err := h.svc.Authenticate(r.Context(), strings.TrimSpace(token))
		if err != nil {
			writeError(w, r, h.log, err)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), principalKey{}, p)))
	})
}

// apiHeaders sets headers every API response should carry. no-store matters
// most: responses can contain a freshly generated signing secret, which no
// cache or proxy may keep.
func apiHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		next.ServeHTTP(w, r)
	})
}
