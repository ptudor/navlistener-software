package control

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ptudor/navlistener/internal/attestation"
	"github.com/ptudor/navlistener/internal/authorization"
	"github.com/ptudor/navlistener/internal/commissioning"
)

func recommission(t *testing.T, r Request, key *ecdsa.PrivateKey, change func(*commissioning.Statement)) Request {
	t.Helper()
	b, err := hex.DecodeString(r.CommissioningRecord)
	if err != nil {
		t.Fatal(err)
	}
	record, err := commissioning.ParseRecord(b)
	if err != nil {
		t.Fatal(err)
	}
	s, err := record.Statement()
	if err != nil {
		t.Fatal(err)
	}
	change(&s)
	signer, err := commissioning.NewKeySigner(key, nil)
	if err != nil {
		t.Fatal(err)
	}
	record, err = commissioning.Sign(s, signer)
	if err != nil {
		t.Fatal(err)
	}
	r.CommissioningRecord = hex.EncodeToString(record[:])
	return r
}

// otherBoard creates a distinct synthetic hardware unit, never an identity alias.
func otherBoard(t *testing.T, r Request, key *ecdsa.PrivateKey, board, serial byte) Request {
	t.Helper()
	h := attestation.HardwareIdentity{Product: r.Product, BoardRevision: r.Revision}
	if err := decodeHex(r.BoardEUI64, h.BoardEUI64[:]); err != nil {
		t.Fatal(err)
	}
	if err := decodeHex(r.ATECCSerial, h.ATECCSerial[:]); err != nil {
		t.Fatal(err)
	}
	h.BoardEUI64[7] = board
	h.ATECCSerial[8] = serial
	core, err := attestation.Sign(1, h, key, nil)
	if err != nil {
		t.Fatal(err)
	}
	r.ObserverID = commissioning.ObserverID(h.BoardEUI64)
	r.BoardEUI64 = hex.EncodeToString(h.BoardEUI64[:])
	r.ATECCSerial = hex.EncodeToString(h.ATECCSerial[:])
	r.CoreRecord = hex.EncodeToString(core[:])
	return recommission(t, r, key, func(s *commissioning.Statement) {
		s.BoardEUI64 = h.BoardEUI64
		s.ATECCSerial = h.ATECCSerial
		s.Attestation = sha256.Sum256(core[:])
	})
}

func TestEnrollmentIdentityUniqueness(t *testing.T) {
	b := newBench(t)
	b.service.DB = testDatabase(t)
	ctx := context.Background()
	if err := b.service.Initialize(ctx, b.config); err != nil {
		t.Fatal(err)
	}
	base := b.request(t, b.ab.ManufacturerKeys[0], b.ab.ManufacturerKeys[1])
	// A model is repeatable; NULL RTC instances do not collide.
	for i := byte(1); i <= 2; i++ {
		r := otherBoard(t, base, b.ab.ManufacturerKeys[0], i, i)
		r = recommission(t, r, b.ab.ManufacturerKeys[1], func(s *commissioning.Statement) {
			s.IdentityFlags = commissioning.IdentityRTCPresent
			s.RTCModel = commissioning.RTCModelDS3231
		})
		if _, _, err := b.service.Enroll(ctx, "operator", r); err != nil {
			t.Fatal(err)
		}
	}
	duplicateATECC := otherBoard(t, base, b.ab.ManufacturerKeys[0], 3, 1)
	if _, _, err := b.service.Enroll(ctx, "operator", duplicateATECC); err == nil {
		t.Fatal("duplicate ATECC accepted")
	}
	for i := byte(4); i <= 5; i++ {
		r := otherBoard(t, base, b.ab.ManufacturerKeys[0], i, i)
		r = recommission(t, r, b.ab.ManufacturerKeys[1], func(s *commissioning.Statement) {
			s.IdentityFlags = 3
			s.RTCModel = commissioning.RTCModelMCP79412
			s.RTCEUI64 = [8]byte{0, 4, 0xa3, 4, 5, 6, 7, 8}
		})
		_, _, err := b.service.Enroll(ctx, "operator", r)
		if (err == nil) != (i == 4) {
			t.Fatalf("bound RTC uniqueness %d: %v", i, err)
		}
	}
}

func TestServiceIdentityAndImmutableEvidence(t *testing.T) {
	b := newBench(t)
	b.service.DB = testDatabase(t)
	ctx := context.Background()
	if err := b.service.Initialize(ctx, b.config); err != nil {
		t.Fatal(err)
	}
	r := b.request(t, b.ab.ManufacturerKeys[0], b.ab.ManufacturerKeys[1])
	id, token, err := b.service.Enroll(ctx, "operator", r)
	if err != nil {
		t.Fatal(err)
	}
	// The actual collector SQL contract must resolve the new authority fields.
	dbConfig := b.service.DB.Config().ConnConfig.ConnString() + " search_path=" + b.service.DB.Config().ConnConfig.RuntimeParams["search_path"]
	provider, err := authorization.NewDatabase(ctx, dbConfig, time.Second, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer provider.Close()
	if err := provider.VerifyContracts(ctx, true, true); err != nil {
		t.Fatal(err)
	}
	got, ok := provider.Authenticate(ctx, token, r.ObserverID, "ubx")
	if !ok || got.ManufacturerAuthorityID != "ab" || got.OperationalAuthorityID != "navlisten" || got.CoreSignerSPKI == "" {
		t.Fatalf("collector contract: %+v %v", got, ok)
	}
	firstCore, _ := hex.DecodeString(r.CoreRecord)
	firstCommission, _ := hex.DecodeString(r.CommissioningRecord)
	r.ReplaceEnrollmentID = id
	// Add a bound RTC, replace it, then switch to a model with no instance ID.
	for _, model := range []commissioning.RTCModel{commissioning.RTCModelMCP79412, commissioning.RTCModelMCP79412, commissioning.RTCModelDS3231} {
		r = recommission(t, r, b.ab.ManufacturerKeys[1], func(s *commissioning.Statement) {
			s.Generation++
			s.IdentityFlags = commissioning.IdentityRTCPresent
			s.RTCModel = model
			s.RTCEUI64 = [8]byte{}
			if model == commissioning.RTCModelMCP79412 {
				s.IdentityFlags |= commissioning.IdentityRTCEUIBound
				s.RTCEUI64 = [8]byte{0, 4, 0xa3, 1, 2, 3, 4, byte(s.Generation)}
			}
		})
		next, _, err := b.service.Enroll(ctx, "operator", r)
		if err != nil {
			t.Fatal(err)
		}
		r.ReplaceEnrollmentID = next
	}
	var core, commission []byte
	if err := b.service.DB.QueryRow(ctx, `SELECT core_record,commissioning_record FROM navl_enrollments WHERE id=$1`, id).Scan(&core, &commission); err != nil || !bytes.Equal(core, firstCore) || !bytes.Equal(commission, firstCommission) {
		t.Fatal("old enrollment evidence changed", err)
	}
	var rtcAbsent bool
	if err := b.service.DB.QueryRow(ctx, `SELECT rtc_eui64 IS NULL FROM navl_devices WHERE observer_id=$1`, r.ObserverID).Scan(&rtcAbsent); err != nil || !rtcAbsent {
		t.Fatal("model-only RTC not null", err)
	}
	// Different bytes at the current generation cannot replace the record.
	bad := recommission(t, r, b.ab.ManufacturerKeys[1], func(s *commissioning.Statement) { s.MCUMAC[5]++ })
	if _, _, err := b.service.Enroll(ctx, "operator", bad); err == nil {
		t.Fatal("reused generation accepted")
	}
	// Recommission a replaced MCU and revoke its previous credential atomically.
	r = recommission(t, r, b.ab.ManufacturerKeys[1], func(s *commissioning.Statement) { s.Generation++; s.MCUMAC[5]++ })
	next, _, err := b.service.Enroll(ctx, "operator", r)
	if err != nil {
		t.Fatal(err)
	}
	r.ReplaceEnrollmentID = next
	if err := b.service.Revoke(ctx, "operator", next); err != nil {
		t.Fatal(err)
	}
	// A stopped/revoked station can be restored only by naming its last enrollment.
	next, _, err = b.service.Enroll(ctx, "operator", r)
	if err != nil {
		t.Fatal(err)
	}
	r.ReplaceEnrollmentID = next
	// ATECC replacement requires a new valid core, a higher generation and an
	// explicit service approval. Ordinary ownership/key changes cannot do this.
	h := attestation.HardwareIdentity{Product: r.Product, BoardRevision: r.Revision}
	if err := decodeHex(r.BoardEUI64, h.BoardEUI64[:]); err != nil {
		t.Fatal(err)
	}
	if err := decodeHex(r.ATECCSerial, h.ATECCSerial[:]); err != nil {
		t.Fatal(err)
	}
	h.ATECCSerial[8]++
	newCore, err := attestation.Sign(1, h, b.ab.ManufacturerKeys[0], nil)
	if err != nil {
		t.Fatal(err)
	}
	r.ATECCSerial = hex.EncodeToString(h.ATECCSerial[:])
	r.CoreRecord = hex.EncodeToString(newCore[:])
	r = recommission(t, r, b.ab.ManufacturerKeys[1], func(s *commissioning.Statement) {
		s.Generation++
		s.ATECCSerial = h.ATECCSerial
		s.Attestation = sha256.Sum256(newCore[:])
	})
	if _, _, err := b.service.Enroll(ctx, "operator", r); err == nil {
		t.Fatal("unapproved core replacement")
	}
	r.ServiceAction = "replace_atecc"
	r.ServiceApproval = "approved-service-1"
	if _, _, err := b.service.Enroll(ctx, "operator", r); err != nil {
		t.Fatal(err)
	}
}

func TestScopedRegistryStreamsAndDatabaseFloors(t *testing.T) {
	b := newBench(t)
	b.service.DB = testDatabase(t)
	ctx := context.Background()
	r := b.request(t, b.ab.ManufacturerKeys[0], b.ab.ManufacturerKeys[1])
	for i, p := range []struct {
		id   string
		key  *ecdsa.PrivateKey
		path string
	}{{"ab", b.ab.RegistryKey, b.ab.RegistryPath}, {"cd", b.cd.RegistryKey, b.cd.RegistryPath}} {
		h := &b.service.Manufacturers[i]
		h.RegistryKeys = []string{p.path}
		h.Registry = filepath.Join(t.TempDir(), "registry.json")
		seq := uint64(1)
		if i == 0 {
			seq = 90
		}
		reg := commissioning.Registry{ManufacturerAuthorityID: p.id, Sequence: seq, IssuedAt: time.Now().UTC(), LedgerHead: strings.Repeat("ab", 32)}
		if i == 0 {
			raw, _ := hex.DecodeString(r.CommissioningRecord)
			reg.Boards = []commissioning.RegistryBoard{{BoardEUI64: r.BoardEUI64, ATECCSerial: r.ATECCSerial, RTCModelID: 0, RTCEUI64: nil, Profile: "open", Generation: 1, Status: "active", Record: base64.StdEncoding.EncodeToString(raw)}}
		}
		signer, _ := commissioning.NewKeySigner(p.key, nil)
		data, err := commissioning.SignRegistry(reg, signer)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(h.Registry, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := b.service.Initialize(ctx, b.config); err != nil {
		t.Fatal(err)
	}
	id, _, err := b.service.Enroll(ctx, "operator", r)
	if err != nil {
		t.Fatal(err)
	}
	r.ReplaceEnrollmentID = id
	if _, err := b.service.DB.Exec(ctx, `UPDATE navl_registry_floors SET sequence=91 WHERE manufacturer_authority_id='ab'`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := b.service.Enroll(ctx, "operator", r); err == nil {
		t.Fatal("persistent floor ignored")
	}
	// C/D's sequence 1 is independent of A/B's 91; software need not claim a
	// manufacturer. A different hardware identity exercises the second stream.
	cd := b.request(t, b.cd.ManufacturerKeys[0], b.cd.ManufacturerKeys[1])
	cd.OperationalAuthorityID = "customer"
	cd.ManufacturerAuthorityID = "cd"
	cd = otherBoard(t, cd, b.cd.ManufacturerKeys[0], 90, 90)
	if _, _, err := b.service.Enroll(ctx, "operator", cd); err != nil {
		t.Fatal("C/D inherited A/B floor", err)
	}
	abRegistry, err := os.ReadFile(b.service.Manufacturers[0].Registry)
	if err != nil {
		t.Fatal(err)
	}
	cdVerifier, err := b.service.Manufacturers[1].NewVerifier()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cdVerifier.LoadRegistry(abRegistry); err == nil {
		t.Fatal("A/B registry accepted in C/D namespace")
	}
	// A C/D signature does not authorize a payload naming A/B either.
	wrong := commissioning.Registry{ManufacturerAuthorityID: "ab", Sequence: 100, IssuedAt: time.Now().UTC(), LedgerHead: strings.Repeat("ab", 32)}
	signer, _ := commissioning.NewKeySigner(b.cd.RegistryKey, nil)
	data, err := commissioning.SignRegistry(wrong, signer)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cdVerifier.LoadRegistry(data); err == nil {
		t.Fatal("payload authority selected its own namespace")
	}
}

func TestOperatorHTTPBoundary(t *testing.T) {
	b := newBench(t)
	token := strings.Repeat("a", 40)
	digest := sha256.Sum256([]byte(token))
	handler := b.service.Handler("operator", hex.EncodeToString(digest[:]))
	valid := Request{ObserverID: "software", OperationalAuthorityID: "customer", OrganizationID: "owner", CollectorInstanceID: "collector", FeedGrants: []string{"ubx"}}
	body, _ := json.Marshal(valid)
	for _, tt := range []struct {
		name, auth, path, body string
		status                 int
	}{
		{"unauthenticated", "", "/v1/enrollments/validate", string(body), http.StatusUnauthorized},
		{"valid", token, "/v1/enrollments/validate", string(body), http.StatusOK},
		{"duplicate", token, "/v1/enrollments/validate", `{"observer_id":"a","observer_id":"b"}`, http.StatusBadRequest},
		{"unknown field", token, "/v1/enrollments/validate", `{"hardware_trust":"trusted"}`, http.StatusBadRequest},
		{"oversize", token, "/v1/enrollments/validate", strings.Repeat(" ", 65<<10), http.StatusBadRequest},
		{"trailing", token, "/v1/enrollments/validate", string(body) + "{}", http.StatusBadRequest},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest("POST", tt.path, strings.NewReader(tt.body))
			r.Header.Set("Authorization", "Bearer "+tt.auth)
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)
			if w.Code != tt.status {
				t.Fatalf("%d %s", w.Code, w.Body.String())
			}
			if w.Header().Get("Cache-Control") != "no-store" {
				t.Fatal("cacheable credential response")
			}
		})
	}
}
