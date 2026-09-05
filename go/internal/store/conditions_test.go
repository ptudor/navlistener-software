package store

import (
	"context"
	"fmt"
	"github.com/ptudor/navlistener/internal/config"
	"testing"
	"time"
)

func TestIntegrationCurrentConditionsRetainsOldActivation(t *testing.T) {
	ctx := context.Background()
	s, err := New(ctx, config.Store{DSN: testDSN(t), RawRetention: "7 days", CompressAfter: "1 day"}, integrationLog())
	if err != nil {
		t.Fatal(err)
	}
	defer s.pool.Close()
	audience := fmt.Sprintf("organization:condition-test-%d", time.Now().UnixNano())
	at := time.Now().Add(-72 * time.Hour)
	put := func(station, value string, when time.Time) int64 {
		t.Helper()
		id, err := s.WriteEvent(ctx, EventRow{Audience: audience, Time: when, SV: station, Type: "jamming_detected", NewValue: value, Severity: 1, DedupeKey: fmt.Sprintf("%s/%s/%s", audience, station, value)})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	first := put("old-active", "jammed", at)
	for i := 0; i < 150; i++ {
		put(fmt.Sprintf("other-%d", i), "ok", at.Add(70*time.Hour))
	}
	snapshot, err := s.CurrentConditions(ctx, audience, time.Time{})
	if err != nil || len(snapshot.Events) != 151 || snapshot.Cursor != 151 {
		t.Fatalf("snapshot %v rows=%d cursor=%d", err, len(snapshot.Events), snapshot.Cursor)
	}
	if snapshot.Events[0].ID != first || snapshot.Events[0].NewValue != "jammed" {
		t.Fatal("old active condition lost")
	}
	resolved := put("old-active", "ok", at.Add(time.Hour)) // commit order, despite older event time
	snapshot, err = s.CurrentConditions(ctx, audience, time.Time{})
	if err != nil || snapshot.Events[len(snapshot.Events)-1].ID != resolved || snapshot.Events[len(snapshot.Events)-1].NewValue != "ok" {
		t.Fatal("resolution tombstone lost", err)
	}
	scoped, err := s.CurrentConditions(ctx, audience, at.Add(2*time.Hour))
	if err != nil || len(scoped.Events) != 150 {
		t.Fatal("policy cutoff not applied", err)
	}
	empty, err := s.CurrentConditions(ctx, audience+"-different", time.Time{})
	if err != nil || empty.Cursor != 0 || len(empty.Events) != 0 {
		t.Fatal("audience disclosure", err)
	}
	// More than the documented bound must fail as a whole, not claim a complete
	// snapshot containing a misleading subset of current conditions.
	_, err = s.pool.Exec(ctx, `INSERT INTO gnss_events(time,audience,audience_seq,sv,event_type,new_value,severity)
 SELECT $1,$2,1000+n,'bound-'||n,'station_offline','offline',1 FROM generate_series(1,$3) n`, time.Now(), audience, MaxCurrentConditions+1)
	if err != nil {
		t.Fatal(err)
	}
	bounded, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if _, err = s.CurrentConditions(bounded, audience, time.Time{}); err == nil {
		t.Fatal("oversized snapshot silently truncated")
	}
}
