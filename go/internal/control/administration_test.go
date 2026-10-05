package control

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ptudor/navlistener/internal/identity"
)

func operatorRequest(t *testing.T, handler http.Handler, token, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("POST", path, strings.NewReader(string(data)))
	r.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	return w
}

// TestPolicyChangeKeepsCredential: changing collections and publication keeps
// the device's token and enrollment, requires a new policy revision, and keeps
// the previous snapshot in the service history.
func TestPolicyChangeKeepsCredential(t *testing.T) {
	db := testDatabase(t)
	b := newBench(t)
	b.service.DB = db
	ctx := context.Background()
	if err := b.service.Initialize(ctx, b.config); err != nil {
		t.Fatal(err)
	}
	software := Request{ObserverID: "software", OperationalAuthorityID: "customer", OrganizationID: "owner", CollectorInstanceID: "collector", FeedGrants: []string{"ubx"},
		Publication: identity.PublicationPolicy{Revision: "r1"}}
	id, token, err := b.service.Enroll(ctx, "operator", software)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte(token))
	change := PolicyChange{EnrollmentID: id, FeedGrants: []string{"ubx"}, CollectionIDs: []string{"site-3"},
		Publication: identity.PublicationPolicy{AggregateUse: identity.AggregatePublicAttributed, StationMetadata: identity.MetadataCoarse, EventVisibility: identity.EventsPrivate, RawExport: identity.RawExportDeny, Revision: "r2"}}
	if err := b.service.ChangePolicy(ctx, "operator", change); err != nil {
		t.Fatal(err)
	}
	var enrollment, revision, aggregate string
	var collections []string
	if err := db.QueryRow(ctx, `SELECT enrollment_id,collection_ids,policy_revision,aggregate_use FROM navlistener_observer_authorization_v3 WHERE token_sha256=$1`, hex.EncodeToString(digest[:])).Scan(&enrollment, &collections, &revision, &aggregate); err != nil {
		t.Fatalf("the device credential no longer authorizes: %v", err)
	}
	if enrollment != id || len(collections) != 1 || collections[0] != "site-3" || revision != "r2" || aggregate != string(identity.AggregatePublicAttributed) {
		t.Fatalf("authorization = %s %v %s %s", enrollment, collections, revision, aggregate)
	}
	var previous string
	if err := db.QueryRow(ctx, `SELECT detail->'previous'->'Publication'->>'policy_revision' FROM navl_service_events WHERE enrollment_id=$1 AND kind='policy'`, id).Scan(&previous); err != nil || previous != "r1" {
		t.Fatalf("previous policy not kept: %q %v", previous, err)
	}

	var bad invalidRequest
	if err := b.service.ChangePolicy(ctx, "operator", change); !errors.As(err, &bad) {
		t.Fatalf("unchanged revision: err = %v, want a request error", err)
	}
	unknown := change
	unknown.EnrollmentID, unknown.Publication.Revision = strings.Repeat("0", 32), "r3"
	if err := b.service.ChangePolicy(ctx, "operator", unknown); !errors.As(err, &bad) {
		t.Fatalf("unknown enrollment: err = %v, want a request error", err)
	}
	public := change
	public.Publication.Revision, public.FeedGrants = "r3", []string{"sbf"}
	if err := b.service.ChangePolicy(ctx, "operator", public); !errors.As(err, &bad) {
		t.Fatalf("unsupported feed: err = %v, want a request error", err)
	}

	operatorToken := strings.Repeat("a", 40)
	opDigest := sha256.Sum256([]byte(operatorToken))
	handler := b.service.Handler("operator", hex.EncodeToString(opDigest[:]))
	w := operatorRequest(t, handler, operatorToken, "/v1/enrollments/policy", change)
	if w.Code != http.StatusUnprocessableEntity || !strings.Contains(w.Body.String(), "policy_revision") {
		t.Fatalf("HTTP unchanged revision: %d %s", w.Code, w.Body.String())
	}
	change.Publication.Revision = "r3"
	if w := operatorRequest(t, handler, operatorToken, "/v1/enrollments/policy", change); w.Code != http.StatusNoContent {
		t.Fatalf("HTTP policy change: %d %s", w.Code, w.Body.String())
	}

	if err := b.service.Revoke(ctx, "operator", id); err != nil {
		t.Fatal(err)
	}
	change.Publication.Revision = "r4"
	if err := b.service.ChangePolicy(ctx, "operator", change); !errors.As(err, &bad) {
		t.Fatalf("revoked enrollment: err = %v, want a request error", err)
	}
}

// TestReadCredentialLifecycle: a read credential is returned once, stored only
// as its digest, validated like the collector's grants, and disabled by digest.
func TestReadCredentialLifecycle(t *testing.T) {
	db := testDatabase(t)
	b := newBench(t)
	b.service.DB = db
	ctx := context.Background()
	if err := b.service.Initialize(ctx, b.config); err != nil {
		t.Fatal(err)
	}
	operatorToken := strings.Repeat("b", 40)
	opDigest := sha256.Sum256([]byte(operatorToken))
	handler := b.service.Handler("operator", hex.EncodeToString(opDigest[:]))

	w := operatorRequest(t, handler, operatorToken, "/v1/read-credentials", ReadCredentialRequest{PrincipalID: "portal-owner", AudienceGrants: []string{"organization:owner"}, Revision: "2026-10-04.1"})
	if w.Code != http.StatusCreated || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}
	var issued IssuedReadCredential
	if err := json.Unmarshal(w.Body.Bytes(), &issued); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte(issued.Token))
	if len(issued.Token) < 32 || issued.TokenSHA256 != hex.EncodeToString(digest[:]) {
		t.Fatalf("issued = %+v", issued)
	}
	var principal string
	var grants []string
	var enabled bool
	if err := db.QueryRow(ctx, `SELECT principal_id,audience_grants,enabled FROM navlistener_read_authorization_v1 WHERE token_sha256=$1`, issued.TokenSHA256).Scan(&principal, &grants, &enabled); err != nil {
		t.Fatal(err)
	}
	if principal != "portal-owner" || len(grants) != 1 || grants[0] != "organization:owner" || !enabled {
		t.Fatalf("view row = %s %v %t", principal, grants, enabled)
	}

	for name, request := range map[string]ReadCredentialRequest{
		"public grant":     {PrincipalID: "portal-owner", AudienceGrants: []string{"public"}, Revision: "r1"},
		"repeated grant":   {PrincipalID: "portal-owner", AudienceGrants: []string{"organization:owner", "organization:owner"}, Revision: "r1"},
		"no grant":         {PrincipalID: "portal-owner", Revision: "r1"},
		"missing revision": {PrincipalID: "portal-owner", AudienceGrants: []string{"organization:owner"}},
	} {
		if w := operatorRequest(t, handler, operatorToken, "/v1/read-credentials", request); w.Code != http.StatusUnprocessableEntity {
			t.Fatalf("%s: %d %s", name, w.Code, w.Body.String())
		}
	}

	disable := map[string]string{"token_sha256": issued.TokenSHA256}
	if w := operatorRequest(t, handler, operatorToken, "/v1/read-credentials/disable", disable); w.Code != http.StatusNoContent {
		t.Fatalf("disable: %d %s", w.Code, w.Body.String())
	}
	if err := db.QueryRow(ctx, `SELECT enabled FROM navlistener_read_authorization_v1 WHERE token_sha256=$1`, issued.TokenSHA256).Scan(&enabled); err != nil || enabled {
		t.Fatalf("disabled credential still enabled: %v", err)
	}
	if w := operatorRequest(t, handler, operatorToken, "/v1/read-credentials/disable", disable); w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("second disable: %d %s", w.Code, w.Body.String())
	}
	var events int
	if err := db.QueryRow(ctx, `SELECT count(*) FROM navl_read_credential_events WHERE token_sha256=$1`, issued.TokenSHA256).Scan(&events); err != nil || events != 2 {
		t.Fatalf("events = %d, %v; want create and disable", events, err)
	}
}
