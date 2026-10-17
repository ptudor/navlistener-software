package config

import (
	"fmt"
	"testing"
	"time"

	"github.com/ptudor/navlistener/internal/integrity"
)

func TestInstallationDefaultsAndOverrides(t *testing.T) {
	for _, i := range []integrity.Installation{integrity.FixedStructure, integrity.Pedestrian, integrity.Bicycle, integrity.RoadVehicle, integrity.Aircraft, integrity.PortableTest} {
		cfg, err := loadBody(t, fmt.Sprintf("[[integrity.station]]\nobserver='test'\ninstallation='%s'\n", i))
		if err != nil {
			t.Fatal(err)
		}
		got := cfg.IntegrityStations()["test"]
		want, _ := integrity.InstallationDefaults(i)
		if got.Mode != want.Mode || got.Motion != want.Motion || got.MaxSpeedMPS != want.MaxSpeedMPS {
			t.Fatalf("%s: %+v", i, got)
		}
	}
	cfg, err := loadBody(t, `[[integrity.station]]
observer='test'
installation='portable_test'
max_speed_mps=12
[integrity.station.motion]
max_acceleration_mps2=3
gap='45s'
`)
	if err != nil {
		t.Fatal(err)
	}
	sp := cfg.IntegrityStations()["test"]
	if sp.MaxSpeedMPS != 12 || sp.Motion.MaxAccelerationMPS2 != 3 || sp.Motion.Gap != 45*time.Second || !sp.Motion.ReportedSpeed {
		t.Fatalf("override: %+v", sp)
	}
}

func TestInvalidInstallationPolicies(t *testing.T) {
	for _, body := range []string{
		"installation='boat'",
		"installation='road_vehicle'\nmode='fixed'",
		"installation='pedestrian'\n[integrity.station.motion]\nreset_after_gap=true",
		"installation='portable_test'\n[integrity.station.motion]\ngap='0s'",
		"installation='road_vehicle'\n[integrity.station.motion]\nmax_acceleration_mps2=-1",
		"installation='fixed_structure'\n[integrity.station.fixed]\ndisplacement_m=1",
	} {
		if _, err := loadBody(t, "[[integrity.station]]\nobserver='test'\n"+body); err == nil {
			t.Fatalf("accepted invalid policy: %s", body)
		}
	}
}
