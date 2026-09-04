package authorization

import (
	"context"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgproto3"
	"golang.org/x/text/secure/precis"
)

// Exercise the actual pgx SCRAM constructor, not merely norm in isolation.
// The synthetic backend requests SCRAM and observes its initial response. No
// credential or credential-containing connection error is written to the log.
func TestInvalidUTF8SCRAMPasswordIsDeadlineBounded(t *testing.T) {
	for i, password := range []string{"\xff", "a\xc0\xaf", "\xe0\x80", "\xcc\x80\xff"} {
		_, validationErr := precis.OpaqueString.String(password)
		t.Logf("synthetic case %d rejected by PRECIS: %v", i, validationErr != nil)
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		result := make(chan error, 1)
		go func() {
			c, err := ln.Accept()
			if err != nil {
				result <- err
				return
			}
			defer c.Close()
			c.SetDeadline(time.Now().Add(time.Second))
			backend := pgproto3.NewBackend(c, c)
			if _, err = backend.ReceiveStartupMessage(); err != nil {
				result <- err
				return
			}
			backend.Send(&pgproto3.AuthenticationSASL{AuthMechanisms: []string{"SCRAM-SHA-256"}})
			if err = backend.Flush(); err != nil {
				result <- err
				return
			}
			backend.SetAuthType(pgproto3.AuthTypeSASL)
			msg, err := backend.Receive()
			if err == nil {
				if _, ok := msg.(*pgproto3.SASLInitialResponse); !ok {
					err = fmt.Errorf("unexpected initial message type %T", msg)
				}
			}
			result <- err
		}()
		cfg, err := pgconn.ParseConfig("host=127.0.0.1 sslmode=disable user=synthetic dbname=synthetic")
		if err != nil {
			t.Fatal("parse test config")
		}
		cfg.Port = uint16(ln.Addr().(*net.TCPAddr).Port)
		cfg.Password = password
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		completed := make(chan struct{})
		go func() {
			c, _ := pgconn.ConnectConfig(ctx, cfg)
			if c != nil {
				c.Close(ctx)
			}
			close(completed)
		}()
		select {
		case <-completed:
		case <-time.After(2 * time.Second):
			t.Fatal("SCRAM normalization did not terminate")
		}
		cancel()
		ln.Close()
		if err := <-result; err != nil {
			t.Fatal("SCRAM initial response was not received")
		}
	}
}
