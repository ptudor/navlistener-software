package control

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/ptudor/navlistener/internal/strictjson"
)

// Handler exposes operator-only enrollment. Digest comparison is constant-time;
// request bodies and credentials are never logged, cached, or echoed on errors.
func (s *Service) Handler(operator, tokenSHA256 string) http.Handler {
	expected, err := hex.DecodeString(tokenSHA256)
	if err != nil || len(expected) != 32 {
		panic("operator token hash must be SHA-256")
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		value := r.Header.Get("Authorization")
		token, ok := strings.CutPrefix(value, "Bearer ")
		actual := sha256.Sum256([]byte(token))
		if !ok || len(token) < 32 || subtle.ConstantTimeCompare(expected, actual[:]) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if r.Method != "POST" {
			w.Header().Set("Allow", "POST")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
		decode := func(dst any) bool {
			data, err := io.ReadAll(r.Body)
			if err != nil || strictjson.Decode(data, dst) != nil {
				http.Error(w, "invalid request", http.StatusBadRequest)
				return false
			}
			return true
		}
		switch r.URL.Path {
		case "/v1/enrollments/validate":
			var request Request
			if !decode(&request) {
				return
			}
			v, err := s.Validate(request)
			if !reportRejection(w, err, "validation rejected") {
				return
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(v.Context)
		case "/v1/enrollments":
			var request Request
			if !decode(&request) {
				return
			}
			id, token, err := s.Enroll(r.Context(), operator, request)
			if !reportRejection(w, err, "enrollment rejected") {
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(map[string]string{"enrollment_id": id, "token": token})
		case "/v1/enrollments/current":
			var request struct {
				ObserverID string `json:"observer_id"`
			}
			if !decode(&request) {
				return
			}
			current, err := s.CurrentEnrollment(r.Context(), request.ObserverID)
			if !reportRejection(w, err, "enrollment lookup rejected") {
				return
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(current)
		case "/v1/enrollments/policy":
			var request PolicyChange
			if !decode(&request) {
				return
			}
			if !reportRejection(w, s.ChangePolicy(r.Context(), operator, request), "policy change rejected") {
				return
			}
			w.WriteHeader(http.StatusNoContent)
		case "/v1/read-credentials":
			var request ReadCredentialRequest
			if !decode(&request) {
				return
			}
			issued, err := s.CreateReadCredential(r.Context(), operator, request)
			if !reportRejection(w, err, "read credential rejected") {
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(issued)
		case "/v1/read-credentials/disable":
			var request struct {
				TokenSHA256 string `json:"token_sha256"`
			}
			if !decode(&request) {
				return
			}
			if !reportRejection(w, s.DisableReadCredential(r.Context(), operator, request.TokenSHA256), "read credential disable rejected") {
				return
			}
			w.WriteHeader(http.StatusNoContent)
		case "/v1/enrollments/revoke":
			var request struct {
				EnrollmentID string `json:"enrollment_id"`
			}
			if !decode(&request) {
				return
			}
			if err := s.Revoke(r.Context(), operator, request.EnrollmentID); err != nil {
				http.Error(w, "revocation rejected", http.StatusConflict)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	})
}

// reportRejection answers a failed operation and reports whether it succeeded.
// A request the operator can correct gets its reason (422). A transaction the
// control plane could not complete, or trust material it could not read, is
// the operator's cue to retry or to look at the server, not to re-validate
// (503). Anything else gets only the fixed summary (409), so database and
// internal errors never reach the response.
func reportRejection(w http.ResponseWriter, err error, summary string) bool {
	if err == nil {
		return true
	}
	var bad invalidRequest
	if errors.As(err, &bad) {
		http.Error(w, summary+": "+bad.Error(), http.StatusUnprocessableEntity)
		return false
	}
	var down unavailable
	if errors.As(err, &down) {
		http.Error(w, summary+": "+down.Error(), http.StatusServiceUnavailable)
		return false
	}
	if retryable(err) {
		w.Header().Set("Retry-After", "1")
		http.Error(w, summary+"; the control plane could not complete the transaction, retry the same request", http.StatusServiceUnavailable)
		return false
	}
	http.Error(w, summary, http.StatusConflict)
	return false
}

// retryable reports a failure the same request can be expected to get past:
// a PostgreSQL transaction-rollback condition (SQLSTATE class 40: the
// serialization failure Serializable isolation raises between concurrent
// enrollments, or a deadlock), or a request context that ended first.
func retryable(err error) bool {
	var pg *pgconn.PgError
	if errors.As(err, &pg) && strings.HasPrefix(pg.Code, "40") {
		return true
	}
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}
