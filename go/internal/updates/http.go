package updates

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/ptudor/navlistener/internal/wire"
)

func decodeRequest(data []byte) (map[string]string, error) {
	if !utf8.Valid(data) {
		return nil, errors.New("invalid UTF-8")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return nil, errors.New("expected JSON object")
	}
	values := map[string]string{}
	for decoder.More() {
		token, err = decoder.Token()
		if err != nil {
			return nil, err
		}
		key, ok := token.(string)
		if !ok {
			return nil, errors.New("invalid key")
		}
		if _, ok = values[key]; ok {
			return nil, errors.New("duplicate key")
		}
		switch key {
		case "observer_id", "request_id", "action", "generation", "release":
		default:
			return nil, errors.New("unknown key")
		}
		var value string
		if err = decoder.Decode(&value); err != nil {
			return nil, err
		}
		values[key] = value
	}
	if token, err = decoder.Token(); err != nil || token != json.Delim('}') {
		return nil, errors.New("invalid object end")
	}
	if _, err = decoder.Token(); err != io.EOF {
		return nil, errors.New("unexpected trailing input")
	}
	return values, nil
}
func decimal(s string) (uint64, error) {
	if s == "" || (len(s) > 1 && s[0] == '0') || strings.Trim(s, "0123456789") != "" {
		return 0, errors.New("expected canonical unsigned decimal string")
	}
	return strconv.ParseUint(s, 10, 64)
}
func (m *Manager) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	if m == nil {
		http.Error(w, "update controls are not configured", http.StatusServiceUnavailable)
		return
	}
	if r.Header.Get("Origin") != "" {
		http.Error(w, "browser-origin commands are not accepted", http.StatusForbidden)
		return
	}
	if len(r.Header.Values("Authorization")) != 1 || !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
		http.Error(w, "explicit update credential required", http.StatusUnauthorized)
		return
	}
	var values map[string]string
	if r.Method == http.MethodGet {
		if len(r.URL.Query()) != 1 || len(r.URL.Query()["observer_id"]) != 1 {
			http.Error(w, "expected observer_id", 400)
			return
		}
		values = map[string]string{"observer_id": r.URL.Query().Get("observer_id")}
	} else if r.Method == http.MethodPost && r.URL.RawQuery == "" && r.Header.Get("Content-Type") == "application/json" {
		data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 2048))
		if err != nil {
			http.Error(w, "request too large", 400)
			return
		}
		values, err = decodeRequest(data)
		if err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
	} else {
		w.Header().Set("Allow", "GET, POST")
		http.Error(w, "unsupported request", 405)
		return
	}
	actor, d, ok := m.authorized(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), values["observer_id"])
	if !ok {
		http.Error(w, "update grant required for this enrolled observer", 403)
		return
	}
	var record Record
	if r.Method == http.MethodGet {
		m.mu.Lock()
		record = m.records[d.key()]
		m.mu.Unlock()
	} else {
		if len(values) != 5 || len(values["request_id"]) != 32 || strings.Trim(values["request_id"], "0123456789abcdef") != "" {
			http.Error(w, "request_id must be 32 lowercase hex characters and all five fields are required", 400)
			return
		}
		actions := map[string]uint8{"check": 1, "download": 2, "install": 3, "cancel": 4}
		generation, e1 := decimal(values["generation"])
		release, e2 := decimal(values["release"])
		if e1 != nil || e2 != nil || actions[values["action"]] == 0 {
			http.Error(w, "invalid command fields", 400)
			return
		}
		command := wire.UpdateCommand{Action: actions[values["action"]], Generation: generation, Release: release}
		var err error
		record, err = m.request(actor, d, values["request_id"], command)
		if err != nil {
			http.Error(w, err.Error(), 409)
			return
		}
		w.WriteHeader(http.StatusAccepted)
	}
	state := "requested"
	if record.Command.ID == 0 {
		state = "none"
	} else if record.Status != nil && record.Status.LastCommand >= record.Command.ID {
		state = "accepted"
	} else if record.Command.Expires <= uint64(m.now().Unix()) {
		state = "expired"
	}
	var choice *Choice
	if record.Status != nil {
		choice, _ = m.choice(record.Status.Channel)
	}
	_ = json.NewEncoder(w).Encode(struct {
		RequestStatus string  `json:"request_status"`
		Record        Record  `json:"record"`
		Choice        *Choice `json:"choice,omitempty"`
	}{state, record, choice})
}
