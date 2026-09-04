package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ptudor/navlistener/internal/audience"
	"github.com/ptudor/navlistener/internal/config"
	"github.com/ptudor/navlistener/internal/identity"
	"github.com/ptudor/navlistener/internal/ingest"
	"github.com/ptudor/navlistener/internal/serve"
	"github.com/ptudor/navlistener/internal/state"
	"github.com/ptudor/navlistener/internal/store"
	"github.com/ptudor/navlistener/internal/wire"
)

type policyReadAuth struct{}

func (policyReadAuth) AuthorizeRead(context.Context, string) (identity.ReadPrincipal, bool) {
	return identity.ReadPrincipal{ID: "reader", Revision: "v1", AudienceGrants: []identity.Audience{{Kind: identity.AudienceOrganization, ID: "old-org"}}}, true
}

func TestIntegrationDisconnectedPolicyWithdrawal(t *testing.T) {
	dsn := os.Getenv("NAVLISTENER_TEST_DSN")
	if dsn == "" {
		t.Skip("NAVLISTENER_TEST_DSN not set")
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	historian, err := store.New(context.Background(), config.Store{DSN: dsn, RawRetention: "90 days"}, log)
	if err != nil {
		t.Fatal(err)
	}
	defer historian.Close()
	for _, change := range []string{"private", "transfer", "disabled"} {
		t.Run(change, func(t *testing.T) {
			old := identity.NewPrivateContext("observer", identity.CredentialToken)
			old.OrganizationID = "old-org"
			old.CollectionIDs = []string{"old-fleet"}
			old.FeedGrants = []string{"ubx"}
			old.DeclaredCapabilities = []identity.Signal{{GnssID: 0, SigID: 0}}
			old.Publication.AggregateUse = identity.AggregatePublicAttributed
			old.Publication.StationMetadata = identity.MetadataFull
			old.Publication.EventVisibility = identity.EventsPublic
			auth := &policyTestAuth{}
			auth.current.Store(&old)
			addr, out := policyTestPush(t, auth)
			c, w, flush := policyTestSession(t, addr, "offline-boot", false)
			rec := wire.RawRecord{RecvUnixNs: time.Now().UnixNano(), FrameType: ingest.TelemJammingStats, Raw: ingest.EncodeJammingStats([]ingest.RFBand{{AGC: 100}})}
			if err := wire.WriteFrame(w, wire.Data, wire.EncodeData(1, rec)); err != nil {
				t.Fatal(err)
			}
			flush()
			var frame *ingest.RawFrame
			select {
			case frame = <-out:
			case <-time.After(time.Second):
				t.Fatal("no initial frame")
			}
			c.Close() // No more connections are made after this point.
			live, pub, events := state.New(1), state.New(1), state.New(1)
			registry := audience.NewRegistry(1, nil)
			public := identity.Audience{Kind: identity.AudiencePublic}
			org := identity.Audience{Kind: identity.AudienceOrganization, ID: "old-org"}
			registry.Register(public, pub, nil)
			registry.Register(identity.Audience{Kind: identity.AudienceOperator, ID: identity.LocalCollectorInstance}, live, nil)
			epochs := audience.NewPolicyEpochs(time.Now().Add(-time.Second))
			controller := scopeController{registry: registry, publicEvents: events, epochs: epochs}
			var last atomic.Int64
			decode := func(f *ingest.RawFrame) {
				ch := make(chan *ingest.RawFrame, 1)
				ch <- f
				close(ch)
				decodeLoop(ch, live, pub, events, nil, log, &last, registry, controller.Apply)
			}
			decode(frame)
			api := serve.NewForAudience("127.0.0.1:0", pub, historian, nil, time.Second, time.Second, log, public)
			api.SetPolicyEpochs(epochs)
			api.EnableAudienceSelection(policyReadAuth{}, registry, time.Second)
			controller.AttachServer(api)
			ln, err := api.Listen()
			if err != nil {
				t.Fatal(err)
			}
			go api.Start(ln)
			defer api.Shutdown(context.Background())
			name := "offline-policy-" + change
			for _, selected := range []identity.Audience{public, org} {
				now := time.Now()
				id, err := historian.WriteEvent(context.Background(), store.EventRow{Audience: selected.Key(), Time: now, SV: name, Type: "station_offline", Severity: 2, Message: name, DedupeKey: fmt.Sprintf("%s-%s-%d", name, selected.Key(), now.UnixNano())})
				if err != nil {
					t.Fatal(err)
				}
				api.PublishEventForAudience(selected, serve.EventMsg{ID: id, Time: now.Format(time.RFC3339), SV: name, Type: "station_offline", Message: name})
			}
			read := func(path string, selected identity.Audience) string {
				req, _ := http.NewRequest("GET", "http://"+ln.Addr().String()+path, nil)
				req.Header.Set("X-GNSS-Audience", selected.Key())
				req.Header.Set("Authorization", "Bearer reader")
				client := http.Client{Timeout: time.Second}
				res, err := client.Do(req)
				if err != nil {
					t.Fatal(err)
				}
				defer res.Body.Close()
				b, err := io.ReadAll(res.Body)
				if err != nil || res.StatusCode != 200 {
					t.Fatalf("request failed: %d %v", res.StatusCode, err)
				}
				return string(b)
			}
			for _, selected := range []identity.Audience{public, org} {
				if !strings.Contains(read("/gnss/api/events", selected), name) {
					t.Fatal("persisted fixture missing before withdrawal")
				}
			}
			next := old
			if change == "disabled" {
				auth.current.Store(nil)
			} else {
				if change == "private" {
					next.Publication.AggregateUse = identity.AggregatePrivate
					next.Publication.StationMetadata = identity.MetadataNone
					next.Publication.EventVisibility = identity.EventsPrivate
				} else {
					next.OrganizationID = "new-org"
					next.CollectionIDs = []string{"new-fleet"}
				}
				auth.current.Store(&next)
			}
			select {
			case marker := <-out:
				if marker.ScopeRevocation == nil {
					t.Fatal("not a reset")
				}
				decode(marker)
			case <-time.After(time.Second):
				t.Fatal("offline revocation bound exceeded")
			}
			for _, selected := range []identity.Audience{public, org} {
				if strings.Contains(read("/gnss/api/v2/observers", selected), "\"id\":\"observer\"") {
					t.Fatal("withdrawn feed contribution")
				}
				if strings.Contains(read("/gnss/api/events", selected), name) {
					t.Fatal("withdrawn history contribution")
				}
				var summary struct {
					Data struct {
						Total *int `json:"total_events"`
					}
				}
				if err := json.Unmarshal([]byte(read("/gnss/api/events/summary", selected)), &summary); err != nil {
					t.Fatal(err)
				}
				if summary.Data.Total == nil || *summary.Data.Total != 0 {
					t.Fatal("withdrawn summary contribution")
				}
				ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
				req, _ := http.NewRequestWithContext(ctx, "GET", "http://"+ln.Addr().String()+"/gnss/events", nil)
				req.Header.Set("X-GNSS-Audience", selected.Key())
				req.Header.Set("Authorization", "Bearer reader")
				req.Header.Set("Last-Event-ID", "1")
				res, err := http.DefaultClient.Do(req)
				if err != nil {
					cancel()
					t.Fatal(err)
				}
				body, _ := io.ReadAll(res.Body)
				res.Body.Close()
				cancel()
				if strings.Contains(string(body), name) {
					t.Fatal("withdrawn SSE replay")
				}
			}
		})
	}
}
