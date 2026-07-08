package config

import (
	"strings"
	"testing"
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
