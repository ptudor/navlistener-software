package ingest

import (
	"encoding/base64"
	"fmt"
	"io"
	"mime"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/ptudor/navlistener/internal/config"
)

// NTRIP is RTCM3 carried over an HTTP-shaped caster protocol (docs/CONSTELLATIONS.md §2.3): the
// client GETs a mountpoint with HTTP Basic auth, the caster replies with a status line and
// headers, and the RTCM3 byte stream follows the blank line. navlistener frames, CRC-checks,
// and persists that stream via scanRTCM — a live raw-capture/validation source (e.g. CRTN at
// Scripps/UCSD). Decoding the RTCM ephemeris messages (1019/1020/1041/…) into live SV state is
// a separate follow-on; this connector is the transport.

const (
	// ntripHandshakeTimeout bounds the request/response exchange; a caster that stalls before
	// the stream starts must not hang the reconnect loop.
	ntripHandshakeTimeout = 15 * time.Second
	// ntripMaxHeaderLine bounds one response header line — untrusted-input discipline against a
	// caster that never sends a newline (docs/INTEGRITY.md §9).
	ntripMaxHeaderLine = 4096
	ntripUserAgent     = "NTRIP navlistener/1"
)

// ntripConnect performs the NTRIP client handshake on an already-dialled caster connection:
// it sends the mountpoint GET (with Basic auth when credentials are set), then consumes the
// response status line and headers, leaving conn positioned at the first byte of the response
// body (RTCM3, or -- regression fix -- chunk-framed RTCM3) so the caller can hand it to scanRTCM,
// wrapping in a de-chunking reader first when chunked is true. It returns an error on any
// non-200/ICY response (mountpoint refused, auth rejected, or a SOURCETABLE reply -- regression fix),
// which the caller treats as a reconnect. Deadlines use wall-clock time directly — they gate
// real I/O, not frame stamping.
// ntripHostHeader builds the HTTP/1.1 Host authority for the caster.
//
// the port used to be stripped unconditionally. NTRIP's normal
// port is 2101, not HTTP's 80, so a caster doing authority-based virtual routing
// received `Host: caster.example` for a `caster.example:2101` request and could
// reject it or route it to the wrong mountpoint set. A bracketed IPv6 authority
// also lost its brackets, which is not a legal Host value at all. Only HTTP's own
// default port is elided — this handshake is plain HTTP over the raw dialed
// conn, so 80 is the right default to test against, rather than "remove every
// port".
//
// The result is interpolated straight into the request line's header block, so a
// control character here would inject headers toward the caster. Config
// validation catches a malformed [[ingest]].addr up front; this is the
// interpolation-site guard that makes that impossible regardless of caller.
func ntripHostHeader(addr string) (string, error) {
	if strings.ContainsFunc(addr, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
		return "", fmt.Errorf("ntrip addr %q contains control characters", addr)
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		// No port component: the configured value is already an authority.
		return addr, nil
	}
	if port != "80" {
		// addr already carries brackets for an IPv6 literal, which is exactly the
		// wire form a Host header wants.
		return addr, nil
	}
	if strings.Contains(host, ":") { // IPv6 literal, brackets stripped by the split
		return "[" + host + "]", nil
	}
	return host, nil
}

func ntripConnect(conn net.Conn, src config.Source) (chunked bool, err error) {
	host, err := ntripHostHeader(src.Addr)
	if err != nil {
		return false, err
	}

	var req strings.Builder
	fmt.Fprintf(&req, "GET /%s HTTP/1.1\r\n", src.Mountpoint)
	fmt.Fprintf(&req, "Host: %s\r\n", host)
	req.WriteString("Ntrip-Version: Ntrip/2.0\r\n")
	fmt.Fprintf(&req, "User-Agent: %s\r\n", ntripUserAgent)
	if src.Username != "" || src.Password != "" {
		cred := base64.StdEncoding.EncodeToString([]byte(src.Username + ":" + src.Password))
		fmt.Fprintf(&req, "Authorization: Basic %s\r\n", cred)
	}
	// regression fix (recorded deferral, not an oversight): "Connection: close" on a GET whose
	// response is by design unbounded is semantically odd — the header promises to close
	// after a response that never completes. Most casters ignore it; a spec-strict
	// HTTP/1.1 caster could in principle treat it unhelpfully. Omitting it (default
	// keep-alive) is likely correct, but the NTRIP 2.0 standard (RTCM 10410.1) is not in
	// the reference library (paid, cite-only class like RTCM-10403) and the review gated
	// this change on verifying tolerance against the actual target casters (CRTN) —
	// run that live check before touching this line.
	req.WriteString("Connection: close\r\n\r\n")

	_ = conn.SetWriteDeadline(time.Now().Add(ntripHandshakeTimeout))
	if _, err := io.WriteString(conn, req.String()); err != nil {
		return false, fmt.Errorf("ntrip request: %w", err)
	}
	_ = conn.SetWriteDeadline(time.Time{})

	_ = conn.SetReadDeadline(time.Now().Add(ntripHandshakeTimeout))
	defer conn.SetReadDeadline(time.Time{})

	status, err := readNtripLine(conn)
	if err != nil {
		return false, fmt.Errorf("ntrip response: %w", err)
	}
	if !ntripAccepted(status) {
		return false, fmt.Errorf("ntrip caster refused mountpoint %q: %q", src.Mountpoint, status)
	}
	// Drain the remaining headers; the RTCM3 (or chunk-framed RTCM3) stream begins right
	// after the blank line. regression fix/capture Transfer-Encoding and Content-Type rather
	// than discard every header, since either can turn "200 OK" into something other than a
	// live RTCM3 byte stream.
	contentType := ""
	for {
		line, err := readNtripLine(conn)
		if err != nil {
			return false, fmt.Errorf("ntrip header: %w", err)
		}
		if line == "" {
			break
		}
		name, val, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(name)) {
		case "transfer-encoding":
			chunked = strings.EqualFold(strings.TrimSpace(val), "chunked")
		case "content-type":
			contentType = strings.TrimSpace(val)
		}
	}
	// a v2 caster answering an unknown mountpoint with "200 OK" and the ASCII
	// sourcetable as body is a refusal, not a stream, exactly like the v1 SOURCETABLE status
	// line ntripAccepted already rejects -- it just arrives one layer later, in the body's
	// declared Content-Type rather than the status line. the media type is parsed,
	// not string-compared, so a parameterized "gnss/sourcetable; charset=utf-8" is still a
	// refusal rather than being mistaken for a silent, frameless RTCM stream; a header
	// mime.ParseMediaType cannot parse falls back to the raw string comparison.
	mediaType := contentType
	if parsed, _, err := mime.ParseMediaType(contentType); err == nil {
		mediaType = parsed
	}
	if strings.EqualFold(mediaType, "gnss/sourcetable") {
		return false, fmt.Errorf("ntrip caster returned a sourcetable for mountpoint %q (Content-Type: %s)", src.Mountpoint, contentType)
	}
	// a 200 whose body is declared text/* (a captive-portal/transparent-proxy
	// interstitial, a caster answering a decommissioned mountpoint with an HTML error
	// page) or application/json (a diagnostic) is a refusal-shaped response, not an
	// RTCM3 byte stream — handing it to scanRTCM yields zero frames and a stream of
	// rtcm_crc/rtcm_length errors while SourceUp sits at 1. Deny-list, not allow-list:
	// NTRIP 1.0/ICY casters send no Content-Type at all and NTRIP 2.0 casters send
	// gnss/data (docs/CONSTELLATIONS.md §2.3), and both must keep passing — as must any
	// unanticipated binary type (application/octet-stream). The no-Content-Type
	// impostor variant is covered by the regression fix frame-silence watchdog instead.
	if mt := strings.ToLower(mediaType); strings.HasPrefix(mt, "text/") || mt == "application/json" {
		return false, fmt.Errorf("ntrip caster returned non-stream Content-Type %q for mountpoint %q", contentType, src.Mountpoint)
	}
	return chunked, nil
}

// readNtripLine reads one CRLF-terminated line off conn a byte at a time, so the read stops
// exactly at the header's end and never consumes into the RTCM3 payload that follows. The
// trailing CR is stripped; lines are length-bounded.
func readNtripLine(conn net.Conn) (string, error) {
	buf := make([]byte, 0, 128)
	one := make([]byte, 1)
	for {
		if _, err := io.ReadFull(conn, one); err != nil {
			return "", err
		}
		if one[0] == '\n' {
			return strings.TrimRight(string(buf), "\r"), nil
		}
		buf = append(buf, one[0])
		if len(buf) > ntripMaxHeaderLine {
			return "", fmt.Errorf("header line exceeds %d bytes", ntripMaxHeaderLine)
		}
	}
}

// ntripAccepted reports whether a caster status line grants the stream. A SOURCETABLE reply is
// a 200 that returns the caster's mountpoint list — it means the requested mount was not found,
// so it is a refusal, not a stream. only the two protocols we actually speak are
// accepted — NTRIP 1.0's exact "ICY 200 OK" and NTRIP 2.0's HTTP/1.0 or HTTP/1.1 — and the
// status code is parsed as the exact three-digit token after the protocol version, not
// substring-matched: "HTTP/1.1 1200 Weird" and "HTTP/2 200 OK" must not pass. Anything else
// on a socket we wrote an HTTP/1.1 request into would be misparsed downstream anyway.
func ntripAccepted(status string) bool {
	fields := strings.Fields(status)
	if len(fields) < 2 {
		return false
	}
	if fields[0] == "ICY" {
		return status == "ICY 200 OK"
	}
	if fields[0] != "HTTP/1.0" && fields[0] != "HTTP/1.1" {
		return false
	}
	if len(fields[1]) != 3 {
		return false
	}
	code, err := strconv.Atoi(fields[1])
	return err == nil && code == 200
}
