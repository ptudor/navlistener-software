package ingest

import (
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func networkGolden(t testing.TB) []byte {
	t.Helper()
	s, err := os.ReadFile("../../../testdata/network_health_v1.hex")
	if err != nil {
		t.Fatal(err)
	}
	b, err := hex.DecodeString(strings.Join(strings.Fields(string(s)), ""))
	if err != nil {
		t.Fatal(err)
	}
	return b
}
func TestNetworkGolden(t *testing.T) {
	b := networkGolden(t)
	n, err := decodeNetwork(b, 80000)
	if err != nil {
		t.Fatal(err)
	}
	if n.WiFi.RSSI != nil || n.WiFi.MinRSSI == nil || *n.WiFi.MinRSSI != -81 || n.WiFi.DisconnectReason == nil || *n.WiFi.DisconnectReason != 200 || n.Counters["dns_failures"] != 12 || len(n.Events) != 8 || n.SpoolHighWater != 120 || n.Collector.TXBytes != 1024 {
		t.Fatalf("bad network: %+v", n)
	}
	raw, _ := json.Marshal(n)
	if !strings.Contains(string(raw), `"rx_frame_errors":null`) || !strings.Contains(string(raw), `"retry_count":null`) {
		t.Fatal(string(raw))
	}
	body := make([]byte, 27)
	body[0] = 1
	body[1] = 4
	binary.BigEndian.PutUint64(body[2:], 80000)
	body[24] = 22
	binary.BigEndian.PutUint16(body[25:], 448)
	body = append(body, b...)
	d, err := decodeObserverDetails(body)
	if err != nil || d.Network == nil {
		t.Fatalf("tag 22: %v", err)
	}
}
func TestNetworkRejectsInvalid(t *testing.T) {
	good := networkGolden(t)
	for _, c := range []struct {
		off int
		v   byte
	}{{0, 2}, {2, 5}, {3, 5}, {7, 3}, {10, 3}, {11, 3}, {18, 128}, {106, 1}, {117, 5}, {116, 255}, {252, 9}, {253, 1}, {264, 255}, {265, 5}, {276, 255}} {
		b := append([]byte(nil), good...)
		b[c.off] = c.v
		if _, err := decodeNetwork(b, 80000); err == nil {
			t.Errorf("accepted offset %d", c.off)
		}
	}
	for i := 0; i < len(good); i++ {
		if _, err := decodeNetwork(good[:i], 80000); err == nil {
			t.Fatalf("accepted truncation %d", i)
		}
	}
	if _, err := decodeNetwork(good, 1000); err == nil {
		t.Fatal("accepted future observations")
	}
}
func TestNetworkWiredAndUnknown(t *testing.T) {
	b := make([]byte, 448)
	b[0] = 1
	b[1] = 2 | 16 | 32 | 64
	b[2] = 2
	b[3] = 2
	b[9] = 100
	b[10] = 2
	b[11] = 2
	b[19] = 8
	copy(b[200:], []byte{192, 0, 2, 8, 255, 255, 255, 0, 192, 0, 2, 1})
	n, err := decodeNetwork(b, 1)
	if err != nil {
		t.Fatal(err)
	}
	if n.WiFi.RSSI != nil || n.Ethernet.Speed == nil || *n.Ethernet.Speed != 100 || *n.Ethernet.Duplex != "full" || *n.Ethernet.IPv4.Address != "192.0.2.8" || n.Collector.Interface != "ethernet" {
		t.Fatalf("%+v", n)
	}
}
func FuzzNetwork(f *testing.F) {
	f.Add(networkGolden(f))
	f.Fuzz(func(t *testing.T, b []byte) { _, _ = decodeNetwork(b, 1000000) })
}
