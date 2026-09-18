package wire

import (
	"encoding/hex"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile("../../../common/fixtures/" + name + ".hex")
	if err != nil {
		t.Fatal(err)
	}
	p, err := hex.DecodeString(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatal(err)
	}
	return p
}
func TestUpdateGoldenProtocol(t *testing.T) {
	p := fixture(t, "update-control-v1")
	c, err := DecodeUpdateCommand(p)
	if err != nil {
		t.Fatal(err)
	}
	if c.ID != 9007199254740993 || c.Release != 18446744073709551614 || c.Generation != 0x102030405060708 {
		t.Fatal(c)
	}
	encoded, err := c.Encode()
	if err != nil || string(encoded) != string(p) {
		t.Fatal("command differs from C golden", err)
	}
	statusBytes := fixture(t, "update-status-v1")
	s, err := DecodeUpdateStatus(statusBytes)
	if err != nil {
		t.Fatal(err)
	}
	if s.Running != c.ID || s.Staged != c.Release || s.LastCommand != c.ID || s.Error != "SAFETY_ACK_TIMEOUT" || s.Channel != "canary" || s.State != "waiting-safe" || s.Received != 8192 || s.Total != 12288 || s.Profile != "trusted" {
		t.Fatal(s)
	}
	data, _ := json.Marshal(s)
	if !strings.Contains(string(data), `"staged_release":"18446744073709551614"`) || !strings.Contains(string(data), `"trust_profile":"trusted"`) {
		t.Fatal(string(data))
	}
	for value, name := range TrustProfiles {
		b := append([]byte(nil), statusBytes...)
		b[5] = byte(value)
		if s, err = DecodeUpdateStatus(b); err != nil || s.Profile != name {
			t.Fatalf("profile %d decoded as %v, %v", value, s, err)
		}
	}
	b := append([]byte(nil), statusBytes...)
	b[5] = byte(len(TrustProfiles))
	if _, err = DecodeUpdateStatus(b); err == nil {
		t.Fatal("accepted an unknown trust profile")
	}
	for n := 0; n < 140; n++ {
		if _, err := DecodeUpdateStatus(statusBytes[:n]); err == nil {
			t.Fatalf("accepted truncated status %d", n)
		}
	}
	for _, at := range []int{0, 1, 2, 3, 4, 5, 72} {
		b := append([]byte(nil), statusBytes...)
		b[at] = 255
		if _, err := DecodeUpdateStatus(b); err == nil {
			t.Fatalf("accepted bad field %d", at)
		}
	}
	for n := 0; n < 36; n++ {
		if _, err := DecodeUpdateCommand(p[:n]); err == nil {
			t.Fatalf("accepted truncated command %d", n)
		}
	}
	p[3] = 1
	if _, err := DecodeUpdateCommand(p); err == nil {
		t.Fatal("install cannot be a hint")
	}
}
