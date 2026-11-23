package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCompareRestingWindows(t *testing.T) {
	dir := t.TempDir()
	write := func(name string, high bool, assessments ...[]byte) string {
		t.Helper()
		var b bytes.Buffer
		fmt.Fprintf(&b, "RF evidence=0251%02x\n", map[bool]byte{false: 0, true: 1}[high])
		for _, s := range assessments {
			fmt.Fprintf(&b, "RF evidence=%x\n", s)
		}
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, b.Bytes(), 0600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	signal := func(flags byte, words ...uint32) []byte {
		s := []byte{2, 2, flags, byte(len(words))}
		for _, w := range words {
			s = binary.LittleEndian.AppendUint32(s, w)
		}
		return s
	}
	const jam = 1 << 24
	a := signal(5, 1575420|jam, 1176450|jam)
	b := signal(5, 1575420, 1176450|jam)
	silent := signal(5, 1176450|jam)
	off := signal(0, 1575420)
	original := write("A.log", true, a, a)
	moved := write("B.log", true, b, b)
	back := write("A2.log", true, a, a)
	for _, test := range []struct {
		name string
		args []string
		want []string
	}{
		{"returned", []string{original, moved, back}, []string{"1575.420 MHz flagged/assessed 2/2 0/2 2/2", "1575.420 MHz changed at the second position", "1176.450 MHz flagged/assessed 2/2 2/2 2/2"}},
		{"initial", []string{original, moved}, []string{"Return the connected antenna"}},
		{"unassessed", []string{original, write("missing", true, silent, silent), back}, []string{"1575.420 MHz flagged/assessed 2/2 unassessed 2/2", "do not show a consistent"}},
		{"disabled", []string{original, write("off", true, off, b), back}, []string{"SEC-SIG reports (known/total) 2/2 1/2 2/2", "do not show a consistent"}},
		{"different sensitivity", []string{original, write("setting", false, b, b), back}, []string{"Verify the same detector sensitivity"}},
		{"no assessment", []string{write("only-setting", true), write("setting-only", true)}, []string{"until the receiver assesses frequencies"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			var out bytes.Buffer
			if err := run(append([]string{"compare"}, test.args...), nil, &out); err != nil {
				t.Fatal(err)
			}
			text := strings.Join(strings.Fields(out.String()), " ")
			for _, part := range test.want {
				if !strings.Contains(text, part) {
					t.Errorf("missing %q in %s", part, text)
				}
			}
		})
	}
}
