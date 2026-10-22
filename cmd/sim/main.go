// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 The MDTI Authors

// Command sim runs the offline sandbox: an in-memory stand-in for the part of
// the Lattice REST API MDTI uses, a stub of the OpenAI Decisions API that
// replays recorded answers, and a live map at http://<addr>/. Point the other
// binaries at it with
//
//	LATTICE_ENDPOINT=http://127.0.0.1:8740 OPENAI_BASE_URL=http://127.0.0.1:8740/v1
//
// (any non-empty token values). `make demo-offline` does all of this.
// It listens on loopback only: it has no authentication.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/b45t3rr/lattice-mdti/internal/lattice"
	"github.com/b45t3rr/lattice-mdti/internal/sim"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:8740", "listen address (loopback only)")
	flag.Parse()
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	if err := run(*addr, log); err != nil {
		fmt.Fprintln(os.Stderr, "sim:", err)
		os.Exit(1)
	}
}

func run(addr string, log *slog.Logger) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return err
	}
	if !lattice.IsLoopback(host) {
		return errors.New("the offline sandbox has no authentication: listen on a loopback address only")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: sim.New(log).Handler(), ReadHeaderTimeout: 10 * time.Second}
	log.Info("sim listening", "url", "http://"+ln.Addr().String(), "map", "http://"+ln.Addr().String()+"/")
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
	if err := srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
