package control

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"strings"

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
			if err != nil {
				http.Error(w, err.Error(), http.StatusUnprocessableEntity)
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
			if err != nil {
				http.Error(w, "enrollment rejected; validate evidence and current enrollment before retrying", http.StatusConflict)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(map[string]string{"enrollment_id": id, "token": token})
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
