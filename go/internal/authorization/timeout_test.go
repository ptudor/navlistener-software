package authorization

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ptudor/navlistener/internal/config"
	"github.com/ptudor/navlistener/internal/identity"
	"github.com/ptudor/navlistener/internal/ingest"
	"github.com/ptudor/navlistener/internal/serve"
	"github.com/ptudor/navlistener/internal/state"
	"github.com/ptudor/navlistener/internal/wire"
)

func timeoutProvider() (*Provider, *atomic.Bool) {
	stalled := new(atomic.Bool)
	stalled.Store(true)
	p := newProvider(time.Minute, slog.New(slog.NewTextHandler(io.Discard, nil)), func(ctx context.Context, _, station, _ string) (identity.ObserverContext, bool, error) {
		if stalled.Load() {
			<-ctx.Done()
			return identity.ObserverContext{}, false, ctx.Err()
		}
		c := identity.NewPrivateContext(station, identity.CredentialToken)
		c.FeedGrants = []string{"ubx"}
		return c, true, nil
	})
	p.lookupTimeout = 20 * time.Millisecond
	p.lookupRead = func(ctx context.Context, _ string) (identity.ReadPrincipal, bool, error) {
		if stalled.Load() {
			<-ctx.Done()
			return identity.ReadPrincipal{}, false, ctx.Err()
		}
		return identity.ReadPrincipal{ID: "reader", Revision: "v1", AudienceGrants: []identity.Audience{{Kind: identity.AudienceOperator, ID: identity.LocalCollectorInstance}}}, true, nil
	}
	return p, stalled
}

func TestLookupTimeoutHonorsEarlierDeadlineAndDoesNotCache(t *testing.T) {
	p, stalled := timeoutProvider()
	for _, earlier := range []bool{false, true} {
		ctx := context.Background()
		cancel := func() {}
		if earlier {
			ctx, cancel = context.WithTimeout(ctx, time.Millisecond)
		}
		start := time.Now()
		if _, ok := p.Authenticate(ctx, "token", "observer", "ubx"); ok {
			t.Fatal("timeout authorized")
		}
		if _, ok := p.AuthorizeRead(ctx, "token"); ok {
			t.Fatal("timeout authorized")
		}
		if time.Since(start) > time.Second {
			t.Fatal("unbounded lookup")
		}
		cancel()
	}
	if len(p.observers)+len(p.readers) != 0 {
		t.Fatal("timeout cached")
	}
	stalled.Store(false)
	if _, ok := p.Authenticate(context.Background(), "token", "observer", "ubx"); !ok {
		t.Fatal("healthy observer denied")
	}
	if _, ok := p.AuthorizeRead(context.Background(), "token"); !ok {
		t.Fatal("healthy reader denied")
	}
}

func TestInitialHTTPAuthorizationDeadline(t *testing.T) {
	p, stalled := timeoutProvider()
	s := serve.New("127.0.0.1:0", state.New(1), nil, nil, time.Second, time.Second, p.log)
	s.EnableAudienceSelection(p, nil, time.Second)
	ln, err := s.Listen()
	if err != nil {
		t.Fatal(err)
	}
	go s.Start(ln)
	defer s.Shutdown(context.Background())
	client := http.Client{Timeout: time.Second}
	for _, path := range []string{"/gnss/api/v2/audiences", "/gnss/api/v2/observers", "/gnss/api/events", "/gnss/events"} {
		req, _ := http.NewRequest("GET", "http://"+ln.Addr().String()+path, nil)
		req.Header.Set("Authorization", "Bearer token")
		res, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != 401 {
			t.Fatalf("%s returned %d", path, res.StatusCode)
		}
	}
	stalled.Store(false)
	req, _ := http.NewRequest("GET", "http://"+ln.Addr().String()+"/gnss/api/v2/observers", nil)
	req.Header.Set("Authorization", "Bearer token")
	res, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 200 {
		t.Fatal(res.StatusCode)
	}
}

func TestInitialPushTimeoutReleasesAdmissionSlot(t *testing.T) {
	p, stalled := timeoutProvider()
	fixture := httptest.NewTLSServer(http.NotFoundHandler())
	defer fixture.Close()
	cert := fixture.TLS.Certificates[0]
	key, err := x509.MarshalPKCS8PrivateKey(cert.PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	certPath := filepath.Join(dir, "cert.pem")
	keyPath := filepath.Join(dir, "key.pem")
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Certificate[0]}), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: key}), 0600); err != nil {
		t.Fatal(err)
	}
	s, err := ingest.NewPushServer(config.Push{Addr: "127.0.0.1:0", TLSCert: certPath, TLSKey: keyPath, MaxConns: 1}, make(chan *ingest.RawFrame, 8), p, p.log)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := s.Listen()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Serve(ctx, ln)
	attempt := func(want bool) {
		t.Helper()
		conn, err := tls.Dial("tcp", ln.Addr().String(), &tls.Config{InsecureSkipVerify: true})
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		conn.SetDeadline(time.Now().Add(time.Second))
		if err := wire.WriteMagic(conn); err != nil {
			t.Fatal(err)
		}
		if err := wire.WriteHello(conn, wire.HelloMsg{Token: "token", Station: "observer", Feed: "ubx", Session: "timeout-test"}); err != nil {
			t.Fatal(err)
		}
		ft, b, err := wire.ReadFrame(conn)
		if err != nil || ft != wire.Welcome {
			t.Fatalf("welcome: %v %v", ft, err)
		}
		var w wire.WelcomeMsg
		if err := json.Unmarshal(b, &w); err != nil {
			t.Fatal(err)
		}
		if w.OK != want {
			t.Fatalf("welcome allowed=%v, want %v", w.OK, want)
		}
		if !want { // Server closes it; the caller has not disconnected to cancel SQL.
			if _, err := conn.Read(make([]byte, 1)); err == nil {
				t.Fatal("denied session remained open")
			}
		}
	}
	attempt(false)
	stalled.Store(false)
	attempt(true)
}
