package fleetreport

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ptudor/navlistener/internal/identity"
	"github.com/ptudor/navlistener/internal/wire"
)

const (
	fingerprint = "c0ffee00c0ffee00c0ffee00c0ffee00c0ffee00c0ffee00c0ffee00c0ffee00"
	neo         = "board-0003-00112233445566778899aabbccddeeff"
	stBoard     = "board-0004-a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5"
)

type vectors struct {
	Valid []map[string]any `json:"valid"`
}

func sharedVectors(t *testing.T) vectors {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "..", "common", "fixtures", "field-report-v1.json"))
	if err != nil {
		t.Fatal(err)
	}
	var v vectors
	if err := json.Unmarshal(data, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func decoded(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func at(s string) time.Time {
	v, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return v
}

// The encoder produces exactly the shared vectors the manufacturer's parser accepts.
func TestBuildMatchesTheSharedVectors(t *testing.T) {
	v := sharedVectors(t)
	stable := &wire.UpdateStatus{Channel: "stable", Mode: "install", State: "idle", Profile: "trusted", Running: 17, Error: "OK"}
	built, err := Build("6c5b1f0e-8d1a-4c4e-9f7a-2b3c4d5e6f70", "example-manufacturing", "collector-1",
		at("2026-10-23T14:00:05Z"), []Board{
			{ObserverID: stBoard, HardwareTrust: identity.HardwareTrustNone, LastReceived: at("2026-10-20T08:59:59Z")},
			{ObserverID: neo, HardwareTrust: identity.HardwareTrustTrusted, CommissioningFingerprint: fingerprint,
				LastReceived: at("2026-10-23T13:42:10Z"), Firmware: "0.1.0+1.b8e037c", Update: stable},
		})
	if err != nil {
		t.Fatal(err)
	}
	if got := decoded(t, built.Body); !reflect.DeepEqual(got, v.Valid[0]) {
		t.Fatalf("report\n got %v\nwant %v", got, v.Valid[0])
	}
	empty, err := Build("00000000-0000-4000-8000-000000000000", "example-manufacturing", "", at("2026-10-23T14:00:05Z"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := decoded(t, empty.Body); !reflect.DeepEqual(got, v.Valid[1]) {
		t.Fatalf("empty report\n got %v\nwant %v", got, v.Valid[1])
	}
	rolled := &wire.UpdateStatus{Channel: "lab", Mode: "manual", State: "rolled-back", Profile: "open",
		Running: 1<<64 - 1, Failed: 18, Error: "TRIAL_ROLLED_BACK"}
	maximum, err := Build("6c5b1f0e-8d1a-4c4e-9f7a-2b3c4d5e6f71", "example-manufacturing", "collector-1",
		at("2026-10-23T14:00:05Z"), []Board{{ObserverID: neo, HardwareTrust: identity.HardwareTrustOpen,
			CommissioningFingerprint: fingerprint, LastReceived: at("2026-10-23T13:00:00Z"), Firmware: "0.1.0+1.b8e037c", Update: rolled}})
	if err != nil {
		t.Fatal(err)
	}
	if got := decoded(t, maximum.Body); !reflect.DeepEqual(got, v.Valid[2]) {
		t.Fatalf("64-bit release report\n got %v\nwant %v", got, v.Valid[2])
	}
}

// A board the report cannot carry exactly is left out and counted, never
// reported wrongly; an unprintable firmware string is left as unknown.
func TestBuildLeavesOutWhatItCannotRepresent(t *testing.T) {
	now := time.Now()
	good := Board{ObserverID: neo, HardwareTrust: identity.HardwareTrustTrusted, CommissioningFingerprint: fingerprint, LastReceived: now}
	cases := []Board{
		{ObserverID: "software-station", HardwareTrust: identity.HardwareTrustNone, LastReceived: now},
		{ObserverID: strings.ToUpper(neo), HardwareTrust: identity.HardwareTrustNone, LastReceived: now},
		{ObserverID: "board-0003-ffffffffffffffffffffffffffffffff", HardwareTrust: identity.HardwareTrustNone, LastReceived: now},
		{ObserverID: "board-0001-0011223344556677", HardwareTrust: identity.HardwareTrustNone, LastReceived: now},
		{ObserverID: stBoard, HardwareTrust: identity.HardwareTrustTrusted, LastReceived: now},
		{ObserverID: stBoard, HardwareTrust: identity.HardwareTrustNone, CommissioningFingerprint: fingerprint, LastReceived: now},
		{ObserverID: stBoard, HardwareTrust: "genuine", LastReceived: now},
		{ObserverID: stBoard, HardwareTrust: identity.HardwareTrustNone, LastReceived: now, Update: &wire.UpdateStatus{Error: "lower case"}},
	}
	withBadFirmware := good
	withBadFirmware.Firmware = "v1\x002"
	built, err := Build("6c5b1f0e-8d1a-4c4e-9f7a-2b3c4d5e6f70", "example-manufacturing", "collector-1", now, append(cases, withBadFirmware))
	if err != nil {
		t.Fatal(err)
	}
	if built.Boards != 1 || built.Omitted != len(cases) {
		t.Fatalf("boards %d omitted %d, want 1 and %d", built.Boards, built.Omitted, len(cases))
	}
	boards := decoded(t, built.Body)["boards"].([]any)
	if firmware := boards[0].(map[string]any)["firmware"]; firmware != nil {
		t.Fatalf("unprintable firmware reported as %v", firmware)
	}
}

func TestReportIDsAreRandomUUIDs(t *testing.T) {
	uuid := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	a, err := NewReportID()
	if err != nil {
		t.Fatal(err)
	}
	b, _ := NewReportID()
	if !uuid.MatchString(a) || a == b {
		t.Fatalf("report IDs %q %q", a, b)
	}
}

func tokenFile(t *testing.T, content string, mode os.FileMode) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fleet-report.token")
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestConfigValidation(t *testing.T) {
	token := tokenFile(t, "nfr_0123456789abcdefghijklmnop\n", 0o600)
	authorities := []string{"example-manufacturing"}
	good := Config{URL: "https://records.example.invalid/intake/field-reports/", TokenFile: token, ManufacturerAuthorityID: "example-manufacturing"}
	if err := good.Validate(authorities); err != nil || good.Interval != DefaultInterval {
		t.Fatalf("good config: %v, interval %s", err, good.Interval)
	}
	var off Config
	if err := off.Validate(nil); err != nil || off.Enabled() {
		t.Fatalf("absent table: %v", err)
	}
	bad := map[string]Config{
		"stray fields":        {TokenFile: token},
		"http":                {URL: "http://records.example.invalid/", TokenFile: token, ManufacturerAuthorityID: "example-manufacturing"},
		"credentials in URL":  {URL: "https://user:pw@records.example.invalid/", TokenFile: token, ManufacturerAuthorityID: "example-manufacturing"},
		"unknown authority":   {URL: good.URL, TokenFile: token, ManufacturerAuthorityID: "someone-else"},
		"interval below 1h":   {URL: good.URL, TokenFile: token, ManufacturerAuthorityID: "example-manufacturing", Intervals: "30m"},
		"no token file":       {URL: good.URL, ManufacturerAuthorityID: "example-manufacturing"},
		"readable token file": {URL: good.URL, TokenFile: tokenFile(t, "nfr_0123456789abcdefghijklmnop\n", 0o644), ManufacturerAuthorityID: "example-manufacturing"},
		"short token":         {URL: good.URL, TokenFile: tokenFile(t, "short\n", 0o600), ManufacturerAuthorityID: "example-manufacturing"},
		"two lines":           {URL: good.URL, TokenFile: tokenFile(t, "nfr_0123456789abcdefghij\nsecond\n", 0o600), ManufacturerAuthorityID: "example-manufacturing"},
	}
	for name, c := range bad {
		if err := c.Validate(authorities); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

type fakeSource struct {
	mu     sync.Mutex
	boards []Board
}

func (f *fakeSource) FleetBoards(string) []Board {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Board(nil), f.boards...)
}

func (f *fakeSource) set(boards ...Board) {
	f.mu.Lock()
	f.boards = boards
	f.mu.Unlock()
}

type intake struct {
	mu       sync.Mutex
	ids      []string
	auth     []string
	statuses []int
	server   *httptest.Server
}

func newIntake(t *testing.T, statuses ...int) *intake {
	in := &intake{statuses: statuses}
	in.server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var report struct {
			ReportID string `json:"report_id"`
		}
		_ = json.Unmarshal(body, &report)
		in.mu.Lock()
		in.ids = append(in.ids, report.ReportID)
		in.auth = append(in.auth, r.Header.Get("Authorization")+" "+r.Header.Get("Content-Type"))
		status := http.StatusCreated
		if len(in.statuses) > 0 {
			status, in.statuses = in.statuses[0], in.statuses[1:]
		}
		in.mu.Unlock()
		w.WriteHeader(status)
		fmt.Fprintf(w, `{"status":"%d"}`, status)
	}))
	t.Cleanup(in.server.Close)
	return in
}

func (in *intake) requests() []string {
	in.mu.Lock()
	defer in.mu.Unlock()
	return append([]string(nil), in.ids...)
}

func testReporter(t *testing.T, in *intake, source Source) *Reporter {
	t.Helper()
	cfg := Config{URL: in.server.URL + "/intake/field-reports/", TokenFile: tokenFile(t, "nfr_0123456789abcdefghijklmnop\n", 0o600),
		ManufacturerAuthorityID: "example-manufacturing", Interval: time.Hour}
	r := newReporter(cfg, source, "collector-1", slog.New(slog.NewTextHandler(io.Discard, nil)), in.server.Client())
	r.startDelay, r.sampleEvery, r.firstRetry, r.maxRetry = 10*time.Millisecond, time.Hour, 20*time.Millisecond, 40*time.Millisecond
	return r
}

func waitFor(t *testing.T, what string, done func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !done() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// A failed delivery is retried with the same report ID until answered; the
// bearer token goes in the header.
func TestRetriesKeepTheReportID(t *testing.T) {
	in := newIntake(t, http.StatusServiceUnavailable, http.StatusTooManyRequests, http.StatusCreated)
	source := &fakeSource{}
	source.set(Board{ObserverID: neo, HardwareTrust: identity.HardwareTrustNone, LastReceived: time.Now()})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go testReporter(t, in, source).Run(ctx)
	waitFor(t, "three attempts", func() bool { return len(in.requests()) == 3 })
	ids := in.requests()
	if ids[0] == "" || ids[0] != ids[1] || ids[1] != ids[2] {
		t.Fatalf("report IDs across retries = %v", ids)
	}
	time.Sleep(100 * time.Millisecond)
	if n := len(in.requests()); n != 3 {
		t.Fatalf("%d attempts after acceptance, want 3", n)
	}
	in.mu.Lock()
	defer in.mu.Unlock()
	if in.auth[0] != "Bearer nfr_0123456789abcdefghijklmnop application/json" {
		t.Fatalf("headers = %q", in.auth[0])
	}
}

// A refusal is not retried, and a redirect is a refusal: the report and its
// token never go to another address.
func TestRefusalsAndRedirectsAreNotRetried(t *testing.T) {
	for _, status := range []int{http.StatusBadRequest, http.StatusUnauthorized, http.StatusTemporaryRedirect} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			elsewhere := newIntake(t)
			in := newIntake(t, status)
			if status == http.StatusTemporaryRedirect {
				in.server.Config.Handler = http.RedirectHandler(elsewhere.server.URL, status)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			r := testReporter(t, in, &fakeSource{})
			result, _ := r.attempt(ctx, Built{ID: "x", Body: []byte("{}")})
			if result != refused {
				t.Fatalf("result %s, want refused", result)
			}
			if len(elsewhere.requests()) != 0 {
				t.Fatal("the redirect was followed")
			}
		})
	}
}

// The collector forgets a silent board after an hour; the reporter does not.
func TestABoardOfflineAtReportTimeIsStillListed(t *testing.T) {
	in := newIntake(t)
	source := &fakeSource{}
	earlier := time.Now().Add(-3 * time.Hour)
	source.set(Board{ObserverID: neo, HardwareTrust: identity.HardwareTrustNone, LastReceived: earlier})
	r := testReporter(t, in, source)
	r.sample()
	source.set(Board{ObserverID: stBoard, HardwareTrust: identity.HardwareTrustNone, LastReceived: time.Now()})
	r.sample()
	built, ok := r.build()
	if !ok || built.Boards != 2 {
		t.Fatalf("built %+v", built)
	}
	// An older sighting never replaces a newer one.
	source.set(Board{ObserverID: neo, HardwareTrust: identity.HardwareTrustNone, LastReceived: earlier.Add(-time.Hour)})
	r.sample()
	if !r.seen[neo].LastReceived.Equal(earlier) {
		t.Fatalf("kept %v, want %v", r.seen[neo].LastReceived, earlier)
	}
}

func TestTheLongestSilentBoardMakesRoom(t *testing.T) {
	source := &fakeSource{}
	start := time.Now().Add(-48 * time.Hour)
	boards := make([]Board, 0, MaxBoards+1)
	for i := 0; i <= MaxBoards; i++ {
		boards = append(boards, Board{ObserverID: fmt.Sprintf("board-0003-%032x", i+1), LastReceived: start.Add(time.Duration(i) * time.Second)})
	}
	source.set(boards...)
	r := testReporter(t, newIntake(t), source)
	r.sample()
	if len(r.seen) != MaxBoards {
		t.Fatalf("kept %d boards, want %d", len(r.seen), MaxBoards)
	}
	if _, ok := r.seen[fmt.Sprintf("board-0003-%032x", 1)]; ok {
		t.Fatal("the longest-silent board was kept")
	}
}
