package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/ptudor/navlistener/internal/audience"
	"github.com/ptudor/navlistener/internal/config"
	"github.com/ptudor/navlistener/internal/detect"
	"github.com/ptudor/navlistener/internal/identity"
	"github.com/ptudor/navlistener/internal/serve"
	"github.com/ptudor/navlistener/internal/state"
	"github.com/ptudor/navlistener/internal/store"
)

type operatorReadAuth struct{ operator identity.Audience }

func (a operatorReadAuth) AuthorizeRead(context.Context, string) (identity.ReadPrincipal, bool) {
	return identity.ReadPrincipal{ID: "operator-reader", Revision: "v1", AudienceGrants: []identity.Audience{a.operator}}, true
}
func TestFixedPublishersServingDisabled(t *testing.T) {
	if op, pub := fixedEventPublishers(nil, "collector"); op != nil || pub != nil {
		t.Fatal("disabled server must produce true nil publishers")
	}
}
func TestIntegrationFixedAudiencePublishers(t *testing.T) {
	dsn := os.Getenv("NAVLISTENER_TEST_DSN")
	if dsn == "" {
		t.Skip("requires disposable TimescaleDB NAVLISTENER_TEST_DSN")
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	historian, err := store.New(context.Background(), config.Store{DSN: dsn}, log)
	if err != nil {
		t.Fatal(err)
	}
	defer historian.Close()
	db, err := pgx.Connect(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close(context.Background())
	public := identity.Audience{Kind: identity.AudiencePublic}
	operator := identity.Audience{Kind: identity.AudienceOperator, ID: "publisher-test"}
	for _, def := range []identity.Audience{public, operator} {
		t.Run(def.Key(), func(t *testing.T) {
			registry := audience.NewRegistry(1, nil)
			pub, op := state.New(1), state.New(1)
			registry.Register(public, pub, nil)
			registry.Register(operator, op, nil)
			initial := pub
			if def == operator {
				initial = op
			}
			api := serve.NewForAudience("127.0.0.1:0", initial, historian, nil, time.Second, time.Second, log, def)
			api.EnableAudienceSelection(operatorReadAuth{operator}, registry, time.Second)
			ln, err := api.Listen()
			if err != nil {
				t.Fatal(err)
			}
			go api.Start(ln)
			defer api.Shutdown(context.Background())
			opPublisher, pubPublisher := fixedEventPublishers(api, operator.ID)
			if opPublisher == nil || pubPublisher == nil {
				t.Fatal("selectable audience has no publisher")
			}
			type subscription struct{ body <-chan string }
			subscribe := func(a identity.Audience) subscription {
				ctx, cancel := context.WithTimeout(context.Background(), 350*time.Millisecond)
				req, _ := http.NewRequestWithContext(ctx, "GET", "http://"+ln.Addr().String()+"/gnss/events", nil)
				req.Header.Set("X-GNSS-Audience", a.Key())
				req.Header.Set("Authorization", "Bearer reader")
				res, err := http.DefaultClient.Do(req)
				if err != nil {
					cancel()
					t.Fatal(err)
				}
				if res.StatusCode != 200 {
					cancel()
					res.Body.Close()
					t.Fatalf("SSE status %d", res.StatusCode)
				}
				body := make(chan string, 1)
				go func() { defer cancel(); defer res.Body.Close(); b, _ := io.ReadAll(res.Body); body <- string(b) }()
				return subscription{body}
			}
			opStream, pubStream := subscribe(operator), subscribe(public)
			marker := fmt.Sprintf("publisher-%d", time.Now().UnixNano())
			publish := func(a identity.Audience, p eventPublisher, label string) {
				e := detect.Event{Time: time.Now(), SV: "observer", Type: "station_offline", Severity: 2, Message: marker + label}
				pipeline := newEventPipeline(historian, p, log, a.Key())
				pipeline.enqueue(*prepareEvent(e, historian, log))
				pending, ok := pipeline.head()
				if !ok {
					t.Fatal("missing event")
				}
				if !pipeline.writeSafely(context.Background(), pending, defaultEventRetry, true) {
					t.Fatal("event did not commit")
				}
				var count int
				if err := db.QueryRow(context.Background(), `SELECT count(*) FROM gnss_events WHERE audience=$1 AND message=$2`, a.Key(), e.Message).Scan(&count); err != nil || count != 1 {
					t.Fatalf("persisted event count %d: %v", count, err)
				}
			}
			publish(operator, opPublisher, "-private-only")
			publish(public, pubPublisher, "-public-only")
			opBody, pubBody := <-opStream.body, <-pubStream.body
			if !strings.Contains(opBody, marker+"-private-only") || strings.Contains(opBody, marker+"-public-only") {
				t.Fatalf("operator stream misrouted: %s", opBody)
			}
			if !strings.Contains(pubBody, marker+"-public-only") || strings.Contains(pubBody, marker+"-private-only") {
				t.Fatalf("public stream disclosed private event or missed public event: %s", pubBody)
			}
		})
	}
}
