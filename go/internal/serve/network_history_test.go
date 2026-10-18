package serve

import (
	"context"
	"testing"

	"github.com/ptudor/navlistener/internal/identity"
	"github.com/ptudor/navlistener/internal/store"
)

func TestNetworkHistoryPrivateAndScoped(t *testing.T) {
	scope := identity.Audience{Kind: identity.AudienceOrganization, ID: "org-a"}
	calls := 0
	s := sensorTestServer(scope, func(_ context.Context, q store.ObserverSampleQuery) (store.ObserverSamplePage, error) {
		calls++
		if q.Kind != "network" || q.Audience != scope || q.Observer != "station" {
			t.Fatalf("query: %+v", q)
		}
		return store.ObserverSamplePage{}, nil
	})
	if w := sensorRequest(s, "GET", "observer=station&kind=network", "token", scope.Key()); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if w := sensorRequest(s, "GET", "observer=station&kind=network", "", scope.Key()); w.Code == 200 {
		t.Fatal("anonymous history")
	}
	if calls != 1 {
		t.Fatal("unauthorized request reached historian")
	}
}
