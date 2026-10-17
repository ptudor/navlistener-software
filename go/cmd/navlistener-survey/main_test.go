package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ptudor/navlistener/internal/config"
	"github.com/ptudor/navlistener/internal/integrity"
)

func TestSurveyAcceptanceProducesLoadableConfig(t *testing.T) {
	var trace bytes.Buffer
	for i := 0; i < 12; i++ {
		s := integrity.Solution{Received: time.Unix(1760000000+int64(i), 0), TOW: uint32(i * 1000), UTC: time.Unix(1760000000+int64(i), 0), UTCValid: true, FixType: 3, FixOK: true, NumSV: 12, LatDeg: 35, LonDeg: 140, HeightM: 50, HAccM: .1, VAccM: .15, SAccMPS: .1}
		if err := json.NewEncoder(&trace).Encode(s); err != nil {
			t.Fatal(err)
		}
	}
	var out bytes.Buffer
	args := []string{"-duration", "10s", "-min-samples", "10", "-accept-by", "operator", "-reason", "reviewed candidate", "-site-id", "site-v2", "-observer", "mast"}
	if err := run(args, &trace, &out); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "reference.toml")
	if err := os.WriteFile(path, out.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("invalid generated TOML: %v\n%s", err, out.String())
	}
	sp := cfg.IntegrityStations()["mast"]
	if sp.Reference == nil || sp.Reference.Candidate == nil || sp.Reference.Candidate.Independent || sp.Reference.SiteID != "site-v2" {
		t.Fatalf("lost provenance: %+v", sp)
	}
}
