package orbitref

import (
	"compress/gzip"
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestFetchKeepsLastGoodRosterAndCache(t *testing.T) {
	body := fixture(t)
	status := 200
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		z := gzip.NewWriter(w)
		_, _ = z.Write([]byte(body))
		_ = z.Close()
	}))
	defer server.Close()
	at := time.Date(2024, 1, 10, 13, 0, 0, 0, time.UTC)
	c := New(filepath.Join(t.TempDir(), "reference.json"), 18)
	c.root = server.URL
	if err := c.fetch(context.Background(), at, at); err != nil {
		t.Fatal(err)
	}
	// A later, smaller valid reference cannot erase satellites previously seen.
	body = body[:strings.Index(body, "E21 2024")]
	if err := c.fetch(context.Background(), at, at.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	status = 503
	if err := c.fetch(context.Background(), at, at.Add(2*time.Minute)); err == nil {
		t.Fatal("accepted HTTP error")
	}
	if len(c.Snapshot(at).Satellites) != 5 {
		t.Fatal("download failure/shrink erased roster")
	}
	status, body = 200, "broken"
	if err := c.fetch(context.Background(), at, at.Add(3*time.Minute)); err == nil {
		t.Fatal("accepted corrupt reference")
	}
	restored := New(c.cachePath, 18)
	if err := restored.load(); err != nil {
		t.Fatal(err)
	}
	if len(restored.Snapshot(at.Add(24*time.Hour)).Satellites) != 5 {
		t.Fatal("restart erased reference identities")
	}
}
