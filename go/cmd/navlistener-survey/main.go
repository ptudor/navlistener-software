// navlistener-survey builds a candidate from a receiver-neutral solution trace.
// Explicit acceptance emits configuration for review; it never edits a running
// collector, receiver configuration or an existing reference.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/pelletier/go-toml/v2"
	"github.com/ptudor/navlistener/internal/config"
	"github.com/ptudor/navlistener/internal/integrity"
)

func run(args []string, in io.Reader, out io.Writer) error {
	f := flag.NewFlagSet("navlistener-survey", flag.ContinueOnError)
	duration := f.Duration("duration", 10*time.Minute, "minimum uninterrupted survey duration")
	samples := f.Int("min-samples", 600, "minimum qualified samples")
	dispersion := f.Float64("max-dispersion-m", 1, "maximum 3D RMS scatter in metres")
	accuracy := f.Float64("max-accuracy-m", .2, "maximum reported horizontal/vertical accuracy")
	minSV := f.Int("min-sv", 8, "minimum satellites in each fix")
	operator := f.String("accept-by", "", "explicitly accept the candidate as this operator; emit TOML")
	reason := f.String("reason", "", "acceptance reason")
	siteID := f.String("site-id", "", "new reception power_model_epoch for this site")
	observer := f.String("observer", "", "observer ID for accepted TOML configuration")
	if err := f.Parse(args); err != nil {
		return err
	}
	if f.NArg() != 0 {
		return errors.New("read newline-delimited integrity.Solution JSON from stdin")
	}
	survey, err := integrity.NewSurvey(integrity.SurveyPolicy{Duration: *duration, MinSamples: *samples, MaxDispersionM: *dispersion, MaxAccuracyM: *accuracy, MinSV: *minSV})
	if err != nil {
		return err
	}
	decoder := json.NewDecoder(in)
	decoder.DisallowUnknownFields()
	for {
		var sol integrity.Solution
		err := decoder.Decode(&sol)
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("solution trace: %w", err)
		}
		survey.Apply(sol)
	}
	c := survey.Candidate()
	if c == nil {
		return errors.New("no candidate: insufficient continuous qualified data, excessive uncertainty or dispersion")
	}
	if *operator == "" {
		return json.NewEncoder(out).Encode(c)
	}
	if !config.ValidObserverID(*observer) {
		return errors.New("acceptance requires a valid -observer")
	}
	r, err := c.Pin(*operator, *reason, *siteID)
	if err != nil {
		return err
	}
	station := config.IntegrityStation{Observer: *observer, Mode: "fixed", Installation: integrity.FixedStructure, Position: []float64{c.Position.LatDeg, c.Position.LonDeg, c.Position.HeightM}, Reference: r}
	body, err := toml.Marshal(struct {
		Integrity config.Integrity `toml:"integrity"`
	}{config.Integrity{Stations: []config.IntegrityStation{station}}})
	if err != nil {
		return err
	}
	_, err = out.Write(body)
	return err
}

func main() {
	if err := run(os.Args[1:], os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
