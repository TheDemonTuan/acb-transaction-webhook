package httpapi

import (
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strings"
	"time"

	"github.com/thedemontuan/acb-transaction-webhook/internal/auth"
	"github.com/thedemontuan/acb-transaction-webhook/internal/storage"
)

const credentialBodyLimit = 8 << 10

func isCredentialAPIPath(path string) bool {
	return path == "/api/v1/connection/credentials" || path == "/api/v1/connection/credentials/grant"
}

// Run before authentication so even rejected requests cannot be cached.
func credentialResponseHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isCredentialAPIPath(r.URL.Path) {
			w.Header().Set("Cache-Control", "no-store")
			w.Header().Set("Referrer-Policy", "no-referrer")
			w.Header().Set("X-Frame-Options", "DENY")
		}
		next.ServeHTTP(w, r)
	})
}

// Unlike the general CSRF origin check, credential writes never trust Host or
// forwarded headers as an alternative to the configured public origin.
func (s *Server) requireCredentialOrigin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.cfg.PublicOrigin == "" || r.Header.Get("Origin") != s.cfg.PublicOrigin {
			writeError(w, http.StatusForbidden, "ORIGIN_MISMATCH")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func decodeCredentialRequest(w http.ResponseWriter, r *http.Request, value any) bool {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		writeError(w, http.StatusBadRequest, "INVALID_CREDENTIAL_INPUT")
		return false
	}
	r.Body = http.MaxBytesReader(w, r.Body, credentialBodyLimit)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_CREDENTIAL_INPUT")
		return false
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		writeError(w, http.StatusBadRequest, "INVALID_CREDENTIAL_INPUT")
		return false
	}
	return true
}

func (s *Server) validateACBCredentialGrant(w http.ResponseWriter, r *http.Request) {
	identity, ok := auth.FromContext(r.Context())
	if !ok || identity.Subject == "" {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED")
		return
	}
	if !s.credentialGrantLimiter.allow(identity.Subject, 10, time.Minute, time.Now()) {
		w.Header().Set("Retry-After", "60")
		writeError(w, http.StatusTooManyRequests, "CREDENTIAL_GRANT_RATE_LIMITED")
		return
	}
	var input struct {
		Grant string `json:"grant"`
	}
	if !decodeCredentialRequest(w, r, &input) {
		return
	}
	view, err := s.store.ValidateACBCredentialGrant(r.Context(), input.Grant, identity.Subject)
	if err != nil {
		writeCredentialError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

func (s *Server) saveACBCredentials(w http.ResponseWriter, r *http.Request) {
	identity, ok := auth.FromContext(r.Context())
	if !ok || identity.Subject == "" {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED")
		return
	}
	var input struct {
		Grant                string `json:"grant"`
		ExpectedRevision     int64  `json:"expectedRevision"`
		Username             string `json:"username"`
		Password             string `json:"password"`
		PasswordConfirmation string `json:"passwordConfirmation"`
	}
	if !decodeCredentialRequest(w, r, &input) {
		return
	}
	if strings.ContainsAny(input.Username, "\r\n\x00") || strings.ContainsAny(input.Password, "\r\n\x00") {
		writeError(w, http.StatusBadRequest, "INVALID_CREDENTIAL_INPUT")
		return
	}
	input.Username = strings.TrimSpace(input.Username)
	if input.ExpectedRevision < 1 || input.Username == "" || len(input.Username) > 256 || input.Password == "" || len(input.Password) > 1024 || input.Password != input.PasswordConfirmation {
		writeError(w, http.StatusBadRequest, "INVALID_CREDENTIAL_INPUT")
		return
	}
	revision, err := s.store.SaveACBCredentials(r.Context(), input.Grant, identity.Subject, input.ExpectedRevision, input.Username, input.Password)
	if err != nil {
		writeCredentialError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, struct {
		Saved         bool  `json:"saved"`
		Revision      int64 `json:"revision"`
		RequiresLogin bool  `json:"requiresLogin"`
	}{Saved: true, Revision: revision, RequiresLogin: true})
}

func writeCredentialError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, storage.ErrCredentialGrantExpired):
		writeError(w, http.StatusGone, "CREDENTIAL_GRANT_EXPIRED")
	case errors.Is(err, storage.ErrCredentialsRevisionConflict):
		writeError(w, http.StatusConflict, "CREDENTIALS_REVISION_CONFLICT")
	case errors.Is(err, storage.ErrACBSessionBusy):
		writeError(w, http.StatusConflict, "ACB_SESSION_BUSY")
	case errors.Is(err, storage.ErrInvalidCredentialInput):
		writeError(w, http.StatusBadRequest, "INVALID_CREDENTIAL_INPUT")
	case errors.Is(err, storage.ErrMutationGateLocked):
		w.Header().Set("Retry-After", "5")
		w.Header().Set("X-Mutation-Gate", "LOCKED")
		writeError(w, http.StatusServiceUnavailable, "MUTATION_GATE_LOCKED")
	default:
		// Never echo database, encryption or request errors to a credential client.
		writeError(w, http.StatusServiceUnavailable, "CREDENTIALS_UNAVAILABLE")
	}
}
