package ingest

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/pem"
	"io"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ptudor/navlistener/internal/config"
)

func TestNtripTLSDialVerification(t *testing.T) {
	cert := selfSigned(t)
	caPath := t.TempDir() + "/ca.pem"
	if err := os.WriteFile(caPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Certificate[0]}), 0o600); err != nil {
		t.Fatal(err)
	}
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12})
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	serveHandshake := func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_ = conn.(*tls.Conn).Handshake()
	}

	src := config.Source{Name: "caster", Type: "ntrip", Addr: ln.Addr().String(), NTRIPCAFile: caPath}
	go serveHandshake()
	conn, err := dialSource(context.Background(), src)
	if err != nil {
		t.Fatalf("valid CA and IP SAN rejected: %v", err)
	}
	conn.Close()

	src.NTRIPServerName = "wrong.example"
	go serveHandshake()
	if conn, err := dialSource(context.Background(), src); err == nil {
		conn.Close()
		t.Fatal("wrong TLS hostname accepted")
	}

	src.NTRIPServerName = ""
	src.NTRIPCAFile = ""
	go serveHandshake()
	if conn, err := dialSource(context.Background(), src); err == nil {
		conn.Close()
		t.Fatal("untrusted NTRIP certificate accepted")
	}
}

// fakeCaster runs one NTRIP caster exchange over the server end of a pipe: it reads the client
// request up to the blank line, hands it back on reqCh, then writes reply followed by tail
// (the start of the "RTCM3" stream). It closes when done.
func fakeCaster(server net.Conn, reqCh chan<- string, reply string, tail []byte) {
	defer server.Close()
	br := bufio.NewReader(server)
	var sb strings.Builder
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return
		}
		sb.WriteString(line)
		if line == "\r\n" || line == "\n" {
			break
		}
	}
	reqCh <- sb.String()
	_, _ = io.WriteString(server, reply)
	if len(tail) > 0 {
		_, _ = server.Write(tail)
	}
}

// TestNtripConnect verifies the handshake: the request carries the mountpoint and Basic auth,
// an ICY 200 reply is accepted, and the connection is left positioned at the first stream byte.
func TestNtripConnect(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	reqCh := make(chan string, 1)
	go fakeCaster(server, reqCh, "ICY 200 OK\r\nContent-Type: gnss/data\r\n\r\n", []byte{0xD3, 0x00})

	src := config.Source{Name: "crtn", Type: "ntrip", Addr: "caster.invalid:2101", Mountpoint: "P472_RTCM3", Username: "u", Password: "p"}
	chunked, err := ntripConnect(client, src)
	if err != nil {
		t.Fatalf("handshake failed: %v", err)
	}
	if chunked {
		t.Error("chunked = true, want false (no Transfer-Encoding header)")
	}
	req := <-reqCh
	if !strings.Contains(req, "GET /P472_RTCM3 HTTP/1.1") {
		t.Errorf("request line missing/wrong: %q", req)
	}
	want := "Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte("u:p"))
	if !strings.Contains(req, want) {
		t.Errorf("basic auth header missing: %q", req)
	}
	// The next byte off the client conn must be the RTCM3 preamble — the header was consumed
	// exactly, with no over-read into the stream.
	b := make([]byte, 1)
	_ = client.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := io.ReadFull(client, b); err != nil || b[0] != 0xD3 {
		t.Errorf("stream not positioned at RTCM3 preamble: got %#x err %v", b, err)
	}
}

// TestNtripConnectRefused confirms a SOURCETABLE reply (mountpoint not found) is an error, not
// mistaken for a granted stream.
func TestNtripConnectRefused(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	reqCh := make(chan string, 1)
	go fakeCaster(server, reqCh, "SOURCETABLE 200 OK\r\n\r\n", nil)

	src := config.Source{Name: "crtn", Type: "ntrip", Addr: "caster.invalid:2101", Mountpoint: "NOPE"}
	if _, err := ntripConnect(client, src); err == nil {
		t.Error("SOURCETABLE reply accepted as a stream; want refusal")
	}
	<-reqCh
}

// TestNtripConnectChunked guards a v2 caster answering with Transfer-Encoding:
// chunked must be reported as such so the caller can de-chunk before scanRTCM -- otherwise
// hex chunk-size lines and CRLFs interleave into the RTCM3 byte stream.
func TestNtripConnectChunked(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	reqCh := make(chan string, 1)
	go fakeCaster(server, reqCh,
		"HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\nContent-Type: gnss/data\r\n\r\n",
		[]byte{0xD3, 0x00})

	src := config.Source{Name: "crtn", Type: "ntrip", Addr: "caster.invalid:2101", Mountpoint: "P472_RTCM3"}
	chunked, err := ntripConnect(client, src)
	if err != nil {
		t.Fatalf("handshake failed: %v", err)
	}
	if !chunked {
		t.Error("chunked = false, want true (Transfer-Encoding: chunked present)")
	}
	<-reqCh
}

// TestNtripConnectSourcetableRejected guards a v2 caster answering an unknown
// mountpoint with "200 OK" + Content-Type: gnss/sourcetable is a refusal (zero RTCM
// frames will ever arrive), not a granted stream -- unlike the v1 SOURCETABLE status line,
// this one only shows up in the body's declared Content-Type.
func TestNtripConnectSourcetableRejected(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	reqCh := make(chan string, 1)
	go fakeCaster(server, reqCh, "HTTP/1.1 200 OK\r\nContent-Type: gnss/sourcetable\r\n\r\n", []byte("STR;...\r\n"))

	src := config.Source{Name: "crtn", Type: "ntrip", Addr: "caster.invalid:2101", Mountpoint: "NOPE"}
	if _, err := ntripConnect(client, src); err == nil {
		t.Error("gnss/sourcetable reply accepted as a stream; want refusal")
	}
	<-reqCh
}

// TestNtripConnectParameterizedSourcetableRejected guards a valid
// parameterized or case-varied sourcetable media type is still a refusal — the
// base media type is parsed, not compared as a whole string.
func TestNtripConnectParameterizedSourcetableRejected(t *testing.T) {
	for _, ct := range []string{
		"gnss/sourcetable; charset=utf-8",
		"GNSS/SourceTable",
		"GNSS/SOURCETABLE; Charset=UTF-8",
		"gnss/sourcetable ; boundary=x",
	} {
		client, server := net.Pipe()
		reqCh := make(chan string, 1)
		go fakeCaster(server, reqCh, "HTTP/1.1 200 OK\r\nContent-Type: "+ct+"\r\n\r\n", []byte("STR;...\r\n"))
		src := config.Source{Name: "crtn", Type: "ntrip", Addr: "caster.invalid:2101", Mountpoint: "NOPE"}
		if _, err := ntripConnect(client, src); err == nil {
			t.Errorf("Content-Type %q accepted as a stream; want refusal", ct)
		}
		<-reqCh
		client.Close()
	}
}

// TestNtripConnectDataContentTypeAccepted confirms the fix spec's explicit carve-out: a v1
// caster (no Content-Type at all) and an explicit gnss/data both still pass.
func TestNtripConnectDataContentTypeAccepted(t *testing.T) {
	for _, reply := range []string{
		"ICY 200 OK\r\n\r\n", // v1: no Content-Type
		"HTTP/1.1 200 OK\r\nContent-Type: gnss/data\r\n\r\n",
	} {
		client, server := net.Pipe()
		reqCh := make(chan string, 1)
		go fakeCaster(server, reqCh, reply, []byte{0xD3})
		src := config.Source{Name: "crtn", Type: "ntrip", Addr: "caster.invalid:2101", Mountpoint: "P472_RTCM3"}
		if _, err := ntripConnect(client, src); err != nil {
			t.Errorf("reply %q: handshake failed: %v", reply, err)
		}
		<-reqCh
		client.Close()
	}
}

// TestNtripAccepted spot-checks the status-line classifier.
func TestNtripAccepted(t *testing.T) {
	for _, tt := range []struct {
		status string
		ok     bool
	}{
		{"ICY 200 OK", true},
		{"HTTP/1.1 200 OK", true},
		{"HTTP/1.0 200 OK", true},
		{"SOURCETABLE 200 OK", false},
		{"HTTP/1.1 401 Unauthorized", false},
		{"HTTP/1.1 404 Not Found", false},
		// the code must be the exact three-digit token, not a substring.
		{"HTTP/1.1 1200 Weird", false},
		{"HTTP/1.1 2000 Huge", false},
		{"HTTP/1.1 X200 Bad", false},
		{"HTTP/1.1 404 page mentions 200", false},
		{"200", false},            // no protocol token
		{"GARBAGE 200 OK", false}, // unknown protocol
		// only the protocols we speak — HTTP/1.0, HTTP/1.1, and the exact
		// NTRIP 1.0 "ICY 200 OK" — are accepted.
		{"HTTP/2 200 OK", false},
		{"ICY 1200 Embedded", false},
		{"ICY 2001 Nope", false},
		{"ICY 200 OK extra", false},
		{"ICY 200", false},         // NTRIP 1.0's grant is the exact "ICY 200 OK"
		{"HTTP/1.1  200 OK", true}, // tolerate doubled separator space
		{"HTTP/1.1 200", true},     // reason phrase absent
	} {
		if got := ntripAccepted(tt.status); got != tt.ok {
			t.Errorf("ntripAccepted(%q) = %v, want %v", tt.status, got, tt.ok)
		}
	}
}
