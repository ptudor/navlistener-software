package ingest

import (
	"bufio"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/ptudor/navlistener/internal/config"
)

// the Host header dropped the port unconditionally. NTRIP's
// normal port is 2101, not HTTP's 80, so a caster using authority-based virtual
// routing saw a request for the wrong authority; bracketed IPv6 authorities also
// lost their brackets, which is not a legal Host value.
func TestNTRIPHostAuthority(t *testing.T) {
	for _, tc := range []struct{ addr, want string }{
		// A non-default port must survive — this is the defect.
		{"caster.example:2101", "caster.example:2101"},
		{"192.0.2.10:2101", "192.0.2.10:2101"},
		{"[2001:db8::1]:2101", "[2001:db8::1]:2101"},
		{"caster.example:8080", "caster.example:8080"},
		// HTTP's own default port is the only one elided, and an IPv6 literal
		// keeps its brackets when it is.
		{"caster.example:80", "caster.example"},
		{"192.0.2.10:80", "192.0.2.10"},
		{"[2001:db8::1]:80", "[2001:db8::1]"},
		// Already an authority with no port component.
		{"caster.example", "caster.example"},
	} {
		got, err := ntripHostHeader(tc.addr)
		if err != nil {
			t.Errorf("ntripHostHeader(%q): %v", tc.addr, err)
			continue
		}
		if got != tc.want {
			t.Errorf("ntripHostHeader(%q) = %q, want %q", tc.addr, got, tc.want)
		}
	}
	// The header is interpolated into the request's header block, so a control
	// character would inject headers toward the caster.
	for _, bad := range []string{"caster.example\r\nX-Injected: 1:2101", "cast\ner:2101", "a\x7fb:2101"} {
		if got, err := ntripHostHeader(bad); err == nil {
			t.Errorf("ntripHostHeader(%q) = %q, want a control-character rejection", bad, got)
		}
	}
}

// A caster that only answers for its configured host-and-port must complete the
// handshake — the end-to-end form of the same property.
func TestNTRIPVirtualHostCasterAcceptsConfiguredAuthority(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	// The listener's own authority: a real host:port with a non-default port.
	wantHost := ln.Addr().String()

	type result struct {
		host string
		err  error
	}
	served := make(chan result, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			served <- result{err: err}
			return
		}
		defer c.Close()
		_ = c.SetDeadline(time.Now().Add(5 * time.Second))
		r := bufio.NewReader(c)
		var host string
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				served <- result{err: err}
				return
			}
			line = strings.TrimRight(line, "\r\n")
			if line == "" {
				break
			}
			if h, ok := strings.CutPrefix(line, "Host: "); ok {
				host = h
			}
		}
		if host != wantHost {
			// Virtual-host caster: refuse an authority it does not serve.
			fmt.Fprint(c, "HTTP/1.1 404 Not Found\r\n\r\n")
			served <- result{host: host}
			return
		}
		fmt.Fprint(c, "ICY 200 OK\r\n\r\n")
		served <- result{host: host}
	}()

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	src := config.Source{Name: "caster", Type: "ntrip", Addr: ln.Addr().String(), Mountpoint: "MOUNT"}
	if _, err := ntripConnect(conn, src); err != nil {
		got := <-served
		t.Fatalf("handshake refused by a caster that serves only %q; it received Host %q: %v",
			wantHost, got.host, err)
	}
	got := <-served
	if got.host != wantHost {
		t.Errorf("caster received Host %q, want %q", got.host, wantHost)
	}
}
