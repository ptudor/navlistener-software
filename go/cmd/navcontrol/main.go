// navcontrol is NavListen's operator-only enrollment API. Factory CA performs
// certificate signing separately; this process stores no CA private key.
package main

import (
	"context"
	"encoding/hex"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/ptudor/navlistener/internal/config"
	"github.com/ptudor/navlistener/internal/control"
	"github.com/ptudor/navlistener/internal/identity"
)

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	path := flag.String("config", "", "collector authority configuration")
	listen := flag.String("listen", "127.0.0.1:5581", "loopback operator API address")
	dsnFile := flag.String("dsn-file", "", "private file containing the control-plane PostgreSQL DSN")
	operator := flag.String("operator", "", "audit identity of the authorized operator")
	hash := flag.String("operator-token-sha256", "", "SHA-256 of the operator's bearer token")
	flag.Parse()
	if !identity.ValidScopeID(*operator) {
		return fmt.Errorf("operator id is required")
	}
	b, err := hex.DecodeString(*hash)
	if err != nil || len(b) != 32 {
		return fmt.Errorf("operator token SHA-256 is required")
	}
	host, _, err := net.SplitHostPort(*listen)
	if err != nil {
		return err
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("operator API must bind a loopback IP; use an authenticated TLS reverse proxy for remote access")
	}
	cfg, err := config.Load(*path)
	if err != nil {
		return err
	}
	info, err := os.Stat(*dsnFile)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return fmt.Errorf("DSN file must be private (0600)")
	}
	dsn, err := os.ReadFile(*dsnFile)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	db, err := pgxpool.New(ctx, strings.TrimSpace(string(dsn)))
	if err != nil {
		return err
	}
	defer db.Close()
	svc := &control.Service{DB: db, Authorities: cfg.Authorities, Manufacturers: cfg.ManufacturerAuthorities}
	if err := svc.Initialize(ctx, cfg); err != nil {
		return err
	}
	srv := &http.Server{Addr: *listen, Handler: svc.Handler(*operator, *hash), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 15 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 16 << 10}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(shutdown)
	}()
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}
