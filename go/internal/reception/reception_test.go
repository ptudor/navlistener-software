package reception

import (
	"bytes"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func fixture() Expectation {
	e := Expectation{ID: 41, Issued: 1800000000, RadiusM: 1000, AlarmSeconds: 30, ClearSeconds: 30, MinExpected: 4, MinMissing: 3, MissingPercent: 50}
	for sv := 1; sv <= 12; sv++ {
		e.Entries = append(e.Entries, Entry{0, uint8(sv), Satellite, 31})
	}
	return e
}

func TestEdgeOracle(t *testing.T) {
	cc, err := exec.LookPath("cc")
	if err != nil {
		t.Skip("C compiler unavailable")
	}
	bin := filepath.Join(t.TempDir(), "oracle")
	if out, err := exec.Command(cc, "-std=c11", "-Wall", "-Wextra", "-Werror", "testdata/oracle.c", "-o", bin).CombinedOutput(); err != nil {
		t.Fatalf("C oracle: %s: %v", out, err)
	}
	e := fixture()
	b, err := e.Encode()
	if err != nil {
		t.Fatal(err)
	}
	c := exec.Command(bin)
	c.Stdin = bytes.NewReader(b)
	got, err := c.Output()
	if err != nil {
		t.Fatal(err)
	}
	var m Machine
	var want []byte
	for sec := 1; sec <= 70; sec++ {
		s := Sample{ExpectationID: e.ID, Unix: e.Issued + int64(sec), UptimeMS: uint64(sec) * 1000, Valid: 1}
		s.Matched[0] = 3
		if sec >= 40 {
			s.Matched[0] = 255
			s.Matched[1] = 15
		}
		var badCounts uint8
		s.Expected, s.Observed, s.Valid, badCounts = Counts(e, s)
		s.Alarm = m.Step(s.Valid, badCounts, time.Unix(s.Unix, 0), e.AlarmSeconds, e.ClearSeconds)
		if sec == 31 || sec == 70 {
			want = append(want, s.Encode()...)
		}
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("edge/collector mismatch\ngot %x\nwant %x", got, want)
	}
	first, _ := DecodeSample(got[:SampleSize])
	last, _ := DecodeSample(got[SampleSize:])
	if first.Alarm != 1 || last.Alarm != 0 {
		t.Fatal("missing alarm or recovery")
	}
}

func TestUnknownDoesNotRecover(t *testing.T) {
	var m Machine
	at := time.Unix(1800000000, 0)
	for i := 0; i <= 30; i++ {
		m.Step(1, 1, at.Add(time.Duration(i)*time.Second), 30, 30)
	}
	if m.Alarm != 1 {
		t.Fatal("no alarm")
	}
	for i := 31; i < 100; i++ {
		m.Step(0, 0, at.Add(time.Duration(i)*time.Second), 30, 30)
	}
	if m.Alarm != 1 {
		t.Fatal("unknown cleared alarm")
	}
	m.Step(1, 0, at.Add(100*time.Second), 30, 30)
	m.Step(1, 0, at.Add(131*time.Second), 30, 30)
	if m.Alarm != 1 {
		t.Fatal("sample gap counted as continuous recovery")
	}
}

func TestWireAndTimeBounds(t *testing.T) {
	e := fixture()
	b, _ := e.Encode()
	round, err := Decode(b)
	if err != nil || round.ID != e.ID {
		t.Fatal(err)
	}
	for n := 0; n < len(b); n++ {
		if _, err := Decode(b[:n]); err == nil {
			t.Fatalf("accepted truncation %d", n)
		}
	}
	b = append(b, 0)
	if _, err := Decode(b); err == nil {
		t.Fatal("accepted trailing data")
	}
	e.Entries = append(e.Entries, e.Entries[0])
	if _, err := e.Encode(); err == nil {
		t.Fatal("duplicate entry")
	}
	e = fixture()
	s := Sample{ExpectationID: e.ID, Unix: e.Issued + 300, Valid: 255}
	_, _, valid, bad := Counts(e, s)
	if valid != 0 || bad != 0 {
		t.Fatal("expired forecast evaluated")
	}
	s.Unix = e.Issued - 1
	_, _, valid, _ = Counts(e, s)
	if valid != 0 {
		t.Fatal("future forecast evaluated")
	}
}
