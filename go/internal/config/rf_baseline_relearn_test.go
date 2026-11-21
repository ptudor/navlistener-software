package config

import (
	"strings"
	"testing"
	"time"

	"github.com/ptudor/navlistener/internal/integrity"
)

// TestRFBaselineRelearn: the self-healing hold defaults to a day, takes a duration, and
// "never" restores the baseline that keeps a gain cut until it is discarded. Every other
// operating point stays the standard one, and station installations parse beside it.
func TestRFBaselineRelearn(t *testing.T) {
	hash := func(p integrity.Profile) string { return integrity.ConfigHash(p, integrity.StationProfile{}, nil) }
	standard := integrity.DefaultProfile()
	if standard.AGC.BaselineRelearnAfter != 24*time.Hour {
		t.Fatalf("the standard hold is %s", standard.AGC.BaselineRelearnAfter)
	}
	for body, want := range map[string]time.Duration{
		"": 24 * time.Hour,
		"[integrity]\nrf_baseline_relearn = \"24h\"\n":    24 * time.Hour,
		"[integrity]\nrf_baseline_relearn = \"6h\"\n":     6 * time.Hour,
		"[integrity]\nrf_baseline_relearn = \"72h30m\"\n": 72*time.Hour + 30*time.Minute,
		"[integrity]\nrf_baseline_relearn = \"15m\"\n":    15 * time.Minute,
		"[integrity]\nrf_baseline_relearn = \"never\"\n":  0,
		"[integrity]\nrf_baseline_relearn = \"never\"\n[[integrity.station]]\nobserver = \"roof\"\nmode = \"fixed\"\nposition = [37.4219, -122.0841, 12.5]\n": 0,
	} {
		cfg, err := loadBody(t, body)
		if err != nil {
			t.Fatalf("%q: %v", body, err)
		}
		got := cfg.IntegrityProfile()
		if got.AGC.BaselineRelearnAfter != want {
			t.Errorf("%q: hold %s, want %s", body, got.AGC.BaselineRelearnAfter, want)
		}
		got.AGC.BaselineRelearnAfter = standard.AGC.BaselineRelearnAfter
		if hash(got) != hash(standard) {
			t.Errorf("%q changed an operating point other than the hold", body)
		}
		if err := cfg.IntegrityProfile().Validate(); err != nil {
			t.Errorf("%q: resolved profile invalid: %v", body, err)
		}
		if strings.Contains(body, "integrity.station") && cfg.IntegrityStations()["roof"].Position == nil {
			t.Errorf("%q: the station beside the setting was lost", body)
		}
	}
	for _, value := range []string{"soon", "24", "-1h", "0s", "14m59s", "Never", " never"} {
		if _, err := loadBody(t, "[integrity]\nrf_baseline_relearn = \""+value+"\"\n"); err == nil || !strings.Contains(err.Error(), "rf_baseline_relearn") {
			t.Errorf("rf_baseline_relearn %q: err = %v, want it refused by name", value, err)
		}
	}
	// A profile's hash names the hold, so a station's config_hash shows which rule
	// its assessments were made under.
	latched := standard
	latched.AGC.BaselineRelearnAfter = 0
	if hash(latched) == hash(standard) {
		t.Fatal("a latched profile hashes like the standard one")
	}
	// A configuration never loaded has the standard profile.
	if got := (&Config{}).IntegrityProfile(); got.AGC.BaselineRelearnAfter != 24*time.Hour {
		t.Fatalf("an unloaded configuration's hold is %s", got.AGC.BaselineRelearnAfter)
	}
}

// TestRFBaselineRelearnForReplay: the replay loader resolves the same setting, so a
// replay runs under the rule the collector did.
func TestRFBaselineRelearnForReplay(t *testing.T) {
	cfg, err := LoadForReplay(replayConfigFile(t, "[integrity]\nrf_baseline_relearn = \"never\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.IntegrityProfile().AGC.BaselineRelearnAfter; got != 0 {
		t.Fatalf("replay hold = %s, want never", got)
	}
}
