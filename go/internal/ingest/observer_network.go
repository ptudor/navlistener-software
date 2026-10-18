package ingest

import (
	"encoding/binary"
	"fmt"
	"net/netip"
)

// Network reports are passive, boot-scoped observations. ACK progress measures
// collector durability as well as the path; it is deliberately not called RTT.
type BoardNetwork struct {
	SampledMS         uint64            `json:"sampled_uptime_ms"`
	DefaultInterface  string            `json:"default_interface"`
	WiFi              NetworkWiFi       `json:"wifi"`
	Ethernet          NetworkEthernet   `json:"ethernet"`
	Collector         NetworkCollector  `json:"collector"`
	DNS               []*string         `json:"dns_ipv4"`
	Counters          map[string]uint32 `json:"counters"`
	EventTotal        uint32            `json:"event_total"`
	EventsOverwritten uint32            `json:"events_overwritten"`
	Events            []NetworkEvent    `json:"events"`
	LastError         *NetworkEvent     `json:"last_error"`
	SpoolRecords      uint32            `json:"spool_records"`
	SpoolHighWater    uint32            `json:"spool_high_water_records"`
	SpoolDropped      uint64            `json:"spool_dropped_records"`
	SampleFailures    uint32            `json:"sample_failures"`
}
type NetworkIPv4 struct {
	Address *string `json:"address"`
	Netmask *string `json:"netmask"`
	Gateway *string `json:"gateway"`
}
type NetworkWiFi struct {
	Configured       bool        `json:"configured"`
	Associated       bool        `json:"associated"`
	HasIP            bool        `json:"has_ip"`
	RSSI             *int8       `json:"rssi_dbm"`
	MinRSSI          *int8       `json:"minimum_rssi_dbm"`
	Channel          *uint8      `json:"channel"`
	SecondaryChannel *uint8      `json:"secondary_channel"`
	BSSID            *string     `json:"bssid"`
	IPv4             NetworkIPv4 `json:"ipv4"`
	DisconnectReason *uint16     `json:"last_disconnect_reason"`
	DisconnectMS     *uint64     `json:"last_disconnect_uptime_ms"`
	RSSISamples      uint32      `json:"rssi_samples"`
	WeakSamples      uint32      `json:"weak_rssi_samples"`
	RetryCount       *uint64     `json:"retry_count"` // unsupported by the current public driver API
}
type NetworkEthernet struct {
	Present       bool        `json:"present"`
	LinkUp        bool        `json:"link_up"`
	HasIP         bool        `json:"has_ip"`
	Speed         *uint16     `json:"speed_mbps"`
	Duplex        *string     `json:"duplex"`
	Autoneg       *bool       `json:"autonegotiation"`
	IPv4          NetworkIPv4 `json:"ipv4"`
	RXFrameErrors *uint64     `json:"rx_frame_errors"`
	TXErrors      *uint64     `json:"tx_errors"`
}
type NetworkCollector struct {
	Connected    bool    `json:"connected"`
	ViaTunnel    bool    `json:"via_tunnel"`
	Interface    string  `json:"socket_interface"`
	PeerIPv4     *string `json:"peer_ipv4"`
	ConnectedMS  *uint64 `json:"connected_since_uptime_ms"`
	ACKMS        *uint64 `json:"last_ack_progress_uptime_ms"`
	MaxACKGap    uint32  `json:"maximum_ack_gap_ms"`
	ConnectMS    uint32  `json:"last_connect_duration_ms"`
	MaxConnectMS uint32  `json:"maximum_connect_duration_ms"`
	TXBytes      uint64  `json:"tls_written_bytes"`
	RXBytes      uint64  `json:"tls_read_bytes"`
	SentRecords  uint64  `json:"sent_records_including_replay"`
}
type NetworkEvent struct {
	Sequence  uint32 `json:"sequence"`
	UptimeMS  uint64 `json:"uptime_ms"`
	Kind      string `json:"kind"`
	Interface string `json:"interface"`
	RSSI      *int8  `json:"rssi_dbm"`
	Flags     uint8  `json:"flags"`
	Code      int32  `json:"code"`
	Detail    int32  `json:"detail"`
}

var networkInterfaces = [...]string{"unknown", "wifi", "ethernet", "tunnel", "other"}
var networkEvents = [...]string{"", "wifi_associated", "wifi_disconnected", "wifi_address", "wifi_lost_ip",
	"ethernet_link_up", "ethernet_link_down", "ethernet_address", "ethernet_lost_ip", "route_changed",
	"access_point_changed", "ethernet_mode_changed", "connect_attempt", "collector_connected", "dns_failed",
	"tcp_failed", "tls_failed", "handshake_failed", "authentication_rejected", "read_failed", "write_failed",
	"ack_stalled", "path_switch", "collector_closed", "ethernet_failed", "address_changed"}
var networkCounters = [...]string{"wifi_disconnects", "wifi_associations", "ethernet_link_ups", "ethernet_link_downs",
	"wifi_address_events", "ethernet_address_events", "ip_losses", "connect_attempts", "collector_connections",
	"collector_disconnects", "dns_failures", "tcp_failures", "tls_failures", "handshake_failures", "ack_stalls",
	"read_failures", "write_failures"}

func networkIP(b []byte, valid bool) *string {
	if !valid || binary.BigEndian.Uint32(b) == 0 {
		return nil
	}
	s := netip.AddrFrom4([4]byte{b[0], b[1], b[2], b[3]}).String()
	return &s
}
func networkIPv4(b []byte, valid bool) NetworkIPv4 {
	return NetworkIPv4{networkIP(b, valid), networkIP(b[4:], valid), networkIP(b[8:], valid)}
}
func decodeNetwork(b []byte, uptime uint64) (*BoardNetwork, error) {
	if len(b) != 448 || b[0] != 1 || b[2] > 4 || b[3] > 4 || b[7] > 2 || b[10] > 2 || b[11] > 2 || b[252] > 8 {
		return nil, ErrBadTelemetry
	}
	u16, u32, u64 := binary.BigEndian.Uint16, binary.BigEndian.Uint32, binary.BigEndian.Uint64
	valid := u16(b[18:])
	if valid & ^uint16(31) != 0 || u64(b[20:]) > uptime || u64(b[108:]) > uptime || u64(b[128:]) > uptime || u64(b[136:]) > uptime || u64(b[144:]) > uptime || int(b[116]) >= len(networkEvents) || b[117] > 4 {
		return nil, ErrBadTelemetry
	}
	for _, i := range []int{106, 107, 118, 119, 253, 254, 255} {
		if b[i] != 0 {
			return nil, ErrBadTelemetry
		}
	}
	if (valid&1 != 0 && (b[1]&4 == 0 || b[6] == 0 || b[6] > 196)) || (valid&4 != 0 && b[1]&8 == 0) || (valid&8 != 0 && b[1]&32 == 0) || u32(b[240:]) > u32(b[244:]) || u32(b[236:]) > u32(b[224:]) {
		return nil, ErrBadTelemetry
	}
	n := &BoardNetwork{SampledMS: u64(b[20:]), DefaultInterface: networkInterfaces[b[2]], EventTotal: u32(b[28:]), EventsOverwritten: u32(b[32:]), Counters: make(map[string]uint32), Events: []NetworkEvent{}, SpoolRecords: u32(b[236:]), SpoolHighWater: u32(b[224:]), SpoolDropped: u64(b[228:]), SampleFailures: u32(b[248:])}
	for i, name := range networkCounters {
		n.Counters[name] = u32(b[36+4*i:])
	}
	n.WiFi = NetworkWiFi{Configured: b[1]&1 != 0, Associated: b[1]&4 != 0, HasIP: b[1]&8 != 0, IPv4: networkIPv4(b[188:], valid&4 != 0), RSSISamples: u32(b[244:]), WeakSamples: u32(b[240:])}
	if valid&1 != 0 {
		r := int8(b[4])
		n.WiFi.RSSI = &r
		ch, sc := b[6], b[7]
		n.WiFi.Channel = &ch
		n.WiFi.SecondaryChannel = &sc
		s := fmt.Sprintf("%02x:%02x:%02x:%02x:%02x:%02x", b[12], b[13], b[14], b[15], b[16], b[17])
		n.WiFi.BSSID = &s
	}
	if valid&2 != 0 {
		r := int8(b[5])
		n.WiFi.MinRSSI = &r
	}
	if n.Counters["wifi_disconnects"] != 0 {
		reason, ms := u16(b[104:]), u64(b[108:])
		n.WiFi.DisconnectReason = &reason
		n.WiFi.DisconnectMS = &ms
	}
	n.Ethernet = NetworkEthernet{Present: b[1]&2 != 0, LinkUp: b[1]&16 != 0, HasIP: b[1]&32 != 0, IPv4: networkIPv4(b[200:], valid&8 != 0)}
	speed := u16(b[8:])
	if speed != 0 {
		if speed != 10 && speed != 100 && speed != 1000 {
			return nil, ErrBadTelemetry
		}
		n.Ethernet.Speed = &speed
	}
	if b[10] != 0 {
		d := []string{"", "half", "full"}[b[10]]
		n.Ethernet.Duplex = &d
	}
	if b[11] != 0 {
		a := b[11] == 2
		n.Ethernet.Autoneg = &a
	}
	n.DNS = []*string{networkIP(b[212:], valid&16 != 0), networkIP(b[216:], valid&16 != 0)}
	n.Collector = NetworkCollector{Connected: b[1]&64 != 0, ViaTunnel: b[1]&128 != 0, Interface: networkInterfaces[b[3]], PeerIPv4: networkIP(b[220:], true), MaxACKGap: u32(b[152:]), ConnectMS: u32(b[156:]), MaxConnectMS: u32(b[160:]), TXBytes: u64(b[164:]), RXBytes: u64(b[172:]), SentRecords: u64(b[180:])}
	if n.Collector.Connected {
		ms := u64(b[136:])
		n.Collector.ConnectedMS = &ms
	}
	if ms := u64(b[144:]); ms != 0 {
		n.Collector.ACKMS = &ms
	}
	if b[116] != 0 {
		n.LastError = &NetworkEvent{UptimeMS: u64(b[128:]), Kind: networkEvents[b[116]], Interface: networkInterfaces[b[117]], Code: int32(u32(b[120:])), Detail: int32(u32(b[124:]))}
	}
	var lastSeq uint32
	var lastMS uint64
	for i := 0; i < 8; i++ {
		p := b[256+24*i : 256+24*(i+1)]
		if i >= int(b[252]) {
			for _, v := range p {
				if v != 0 {
					return nil, ErrBadTelemetry
				}
			}
			continue
		}
		ms, seq := u64(p), u32(p[20:])
		if p[8] == 0 || int(p[8]) >= len(networkEvents) || p[9] > 4 || ms > uptime || ms < lastMS || seq <= lastSeq || seq > n.EventTotal {
			return nil, ErrBadTelemetry
		}
		e := NetworkEvent{Sequence: seq, UptimeMS: ms, Kind: networkEvents[p[8]], Interface: networkInterfaces[p[9]], Flags: p[11], Code: int32(u32(p[12:])), Detail: int32(u32(p[16:]))}
		if p[10] != 127 {
			r := int8(p[10])
			e.RSSI = &r
		}
		n.Events = append(n.Events, e)
		lastSeq, lastMS = seq, ms
	}
	if n.EventsOverwritten > n.EventTotal || uint32(len(n.Events)) > n.EventTotal {
		return nil, ErrBadTelemetry
	}
	return n, nil
}
