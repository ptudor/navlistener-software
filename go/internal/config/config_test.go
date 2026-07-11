package config

import (
	"strings"
	"testing"
	"time"
)

const goodHash = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func pushConfig(p Push) *Config {
	c := defaults()
	c.Push = p
	return c
}

// TestPushDisabled: with no addr the push endpoint is a no-op and validation passes,
// with the ack interval defaulted.
func TestPushDisabled(t *testing.T) {
	c := pushConfig(Push{})
	if err := c.finalizePush(); err != nil {
		t.Fatalf("disabled push should validate: %v", err)
	}
	if c.Push.AckInterval <= 0 {
		t.Errorf("ack interval not defaulted: %v", c.Push.AckInterval)
	}
}

// TestSnapshotInterval: an unset snapshot_interval defaults to 5m; an explicit "0s"
// disables it; a valid duration is honoured; garbage is a hard error.
func TestSnapshotInterval(t *testing.T) {
	def := defaults()
	if err := def.finalize(); err != nil {
		t.Fatalf("defaults should finalize: %v", err)
	}
	if def.Serve.SnapshotEvery != 5*time.Minute {
		t.Errorf("unset snapshot_interval = %v, want 5m", def.Serve.SnapshotEvery)
	}

	off := defaults()
	off.Serve.SnapshotEverys = "0s"
	if err := off.finalize(); err != nil || off.Serve.SnapshotEvery != 0 {
		t.Errorf("explicit 0s should disable, got %v (err %v)", off.Serve.SnapshotEvery, err)
	}

	custom := defaults()
	custom.Serve.SnapshotEverys = "2m"
	if err := custom.finalize(); err != nil || custom.Serve.SnapshotEvery != 2*time.Minute {
		t.Errorf("2m not honoured: %v (err %v)", custom.Serve.SnapshotEvery, err)
	}

	bad := defaults()
	bad.Serve.SnapshotEverys = "nonsense"
	if err := bad.finalize(); err == nil {
		t.Error("garbage snapshot_interval accepted, want error")
	}
}

// TestLoggingLevelValidated guards unlike logging.format, logging.level was
// never validated, so a typo ("trace"/"warning") silently mapped to info with no
// diagnostic. The empty-string default and the four real levels must still pass.
func TestLoggingLevelValidated(t *testing.T) {
	for _, level := range []string{"", "debug", "info", "warn", "error"} {
		c := defaults()
		c.Logging.Level = level
		if err := c.finalize(); err != nil {
			t.Errorf("logging.level %q should validate, got %v", level, err)
		}
	}
	for _, bad := range []string{"trace", "warning", "DEBUG", " info"} {
		c := defaults()
		c.Logging.Level = bad
		if err := c.finalize(); err == nil {
			t.Errorf("logging.level %q accepted, want a hard error", bad)
		}
	}
}

// TestStoreIntervalsValidatedAtFinalize guards raw_retention/compress_after
// were only checked when the store actually connected, so `-check-config` reported
// a malformed interval (or an injection attempt into the later policy DDL) as
// valid. finalize (which -check-config runs) must catch it up front, using the
// same config.IntervalRe the store re-checks in applyPolicies.
func TestStoreIntervalsValidatedAtFinalize(t *testing.T) {
	for _, good := range []string{"", "7 days", "1 hour", "30 minutes", "2 weeks"} {
		c := defaults()
		c.Store.RawRetention, c.Store.CompressAfter = good, good
		if err := c.finalize(); err != nil {
			t.Errorf("interval %q should validate, got %v", good, err)
		}
	}
	for _, bad := range []string{"soon", "; DROP TABLE nav_frames;--", "7", "days", "0 days"} {
		c := defaults()
		c.Store.RawRetention = bad
		if err := c.finalize(); err == nil {
			t.Errorf("store.raw_retention %q accepted, want a hard error", bad)
		}
		c = defaults()
		c.Store.CompressAfter = bad
		if err := c.finalize(); err == nil {
			t.Errorf("store.compress_after %q accepted, want a hard error", bad)
		}
	}
}

// TestCapabilitiesParsing: valid "gnss:sig" tuples parse; malformed, out-of-range, and
// duplicate declarations are hard errors so a mistyped fingerprint can't silently disarm the
// plausibility detector.
func TestCapabilitiesParsing(t *testing.T) {
	got, err := parseCapabilities([]string{"0:0", "2:3", " 6 : 0 "})
	if err != nil {
		t.Fatalf("valid capabilities rejected: %v", err)
	}
	want := []Capability{{0, 0}, {2, 3}, {6, 0}}
	if len(got) != len(want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("cap[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
	for _, bad := range []string{"2", "2:", ":3", "8:0", "x:0", "0:999", "2:3:4", ""} {
		if _, err := parseCapabilities([]string{bad}); err == nil {
			t.Errorf("capability %q accepted, want rejected", bad)
		}
	}
	if _, err := parseCapabilities([]string{"2:0", "2:0"}); err == nil {
		t.Error("duplicate capability accepted, want rejected")
	}
}

// TestNtripSource: an ntrip dial source requires a mountpoint; a valid one passes; ntrip is a
// dial type but not a push feed grant.
func TestNtripSource(t *testing.T) {
	c := defaults()
	c.Ingest = []Source{{Name: "crtn", Type: "ntrip", Addr: "caster.invalid:2101"}} // no mountpoint
	if err := c.finalize(); err == nil || !strings.Contains(err.Error(), "mountpoint") {
		t.Fatalf("ntrip without mountpoint should fail on mountpoint, got %v", err)
	}

	ok := defaults()
	ok.Ingest = []Source{{Name: "crtn", Type: "ntrip", Addr: "caster.invalid:2101", Mountpoint: "P472_RTCM3"}}
	if err := ok.finalize(); err != nil {
		t.Fatalf("valid ntrip source rejected: %v", err)
	}

	// ntrip is not a valid push feed grant (a feeder can't push "ntrip").
	if knownIngestTypes["ntrip"] {
		t.Error("ntrip must not be a push feed type")
	}
}

// TestPushRequiresTLS: enabling the endpoint without cert/key is a hard error —
// unauthenticated feeder ingest must never be possible.
func TestPushRequiresTLS(t *testing.T) {
	c := pushConfig(Push{Addr: "0.0.0.0:5580"})
	err := c.finalizePush()
	if err == nil || !strings.Contains(err.Error(), "tls_cert") {
		t.Fatalf("err = %v, want a tls requirement error", err)
	}
}

// TestPushTokenHashValidated: a token hash that isn't 64 hex chars is rejected, so a
// truncated or placeholder credential can't slip into the observer table.
func TestPushTokenHashValidated(t *testing.T) {
	base := Push{Addr: "0.0.0.0:5580", TLSCert: "c.pem", TLSKey: "k.pem"}
	for _, bad := range []string{"deadbeef", strings.Repeat("z", 64)} {
		c := pushConfig(base)
		c.Push.Observers = []PushObserver{{Station: "s", TokenSHA256: bad, Feeds: []string{"ubx"}}}
		if err := c.finalizePush(); err == nil {
			t.Errorf("token %q accepted, want rejected", bad)
		}
	}
}

// TestPushFeedGrantValidated: an observer must name at least one known feed type.
func TestPushFeedGrantValidated(t *testing.T) {
	base := Push{Addr: "0.0.0.0:5580", TLSCert: "c.pem", TLSKey: "k.pem"}
	c := pushConfig(base)
	c.Push.Observers = []PushObserver{{Station: "s", TokenSHA256: goodHash, Feeds: []string{"nmea"}}}
	if err := c.finalizePush(); err == nil {
		t.Error("unknown feed type accepted")
	}
	c.Push.Observers[0].Feeds = nil
	if err := c.finalizePush(); err == nil {
		t.Error("empty feed grant accepted")
	}
}

// TestPushSBFFeedGrantRejected guards GNF1's 1-byte frame_type field
// cannot carry an SBF block number, so an sbf push grant must be rejected at
// config validation until a block-number carriage is defined -- dial-mode sbf
// (a separate, decoder-owned connector) is unaffected.
func TestPushSBFFeedGrantRejected(t *testing.T) {
	c := pushConfig(Push{Addr: "0.0.0.0:5580", TLSCert: "c.pem", TLSKey: "k.pem"})
	c.Push.Observers = []PushObserver{{Station: "s", TokenSHA256: goodHash, Feeds: []string{"sbf"}}}
	if err := c.finalizePush(); err == nil {
		t.Error("sbf push feed grant accepted, want rejected (no block-number carriage yet)")
	}
	// Dial-mode sbf remains valid -- this finding is push-only.
	ok := defaults()
	ok.Ingest = []Source{{Name: "sbf-recv", Type: "sbf", Addr: "127.0.0.1:5555"}}
	if err := ok.finalize(); err != nil {
		t.Errorf("dial-mode sbf source rejected: %v (should be unaffected by the push-only regression fix restriction)", err)
	}
}

// TestPushValid: a complete, well-formed observer table validates.
func TestPushValid(t *testing.T) {
	c := pushConfig(Push{
		Addr: "0.0.0.0:5580", TLSCert: "c.pem", TLSKey: "k.pem", AckIntervals: "500ms",
		Observers: []PushObserver{
			{Station: "observer16", TokenSHA256: goodHash, Feeds: []string{"ubx", "rtcm"}},
		},
	})
	if err := c.finalizePush(); err != nil {
		t.Fatalf("valid push config rejected: %v", err)
	}
	if c.Push.AckInterval.String() != "500ms" {
		t.Errorf("ack interval = %v, want 500ms", c.Push.AckInterval)
	}
}

// TestPushDuplicateStation: two observers with the same station id are rejected.
func TestPushDuplicateStation(t *testing.T) {
	c := pushConfig(Push{
		Addr: "0.0.0.0:5580", TLSCert: "c.pem", TLSKey: "k.pem",
		Observers: []PushObserver{
			{Station: "dup", TokenSHA256: goodHash, Feeds: []string{"ubx"}},
			{Station: "dup", TokenSHA256: goodHash, Feeds: []string{"sbf"}},
		},
	})
	if err := c.finalizePush(); err == nil {
		t.Error("duplicate station accepted")
	}
}
