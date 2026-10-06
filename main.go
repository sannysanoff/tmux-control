package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
)

func defaultHermesHome() string {
	if h := os.Getenv("HERMES_HOME"); h != "" {
		return h
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "/home/ubuntu/.hermes"
	}
	return filepath.Join(home, ".hermes")
}

func main() {
	var (
		mode       = flag.String("mode", "local", "run mode: local | spagetti")
		envFile    = flag.String("env", ".env", "env file for spagetti mode (SPAGETTI_GATEWAY, SPAGETTI_PASSWORD, SPAGETTI_NAME)")
		listen     = flag.String("listen", "127.0.0.1:8790", "HTTP listen address (local mode)")
		pollEvery  = flag.Duration("poll", 2*time.Second, "poll interval for tmux/proc/lease/marker scan")
		turnWindow = flag.Duration("turn-window", 20*time.Second, "agent-log activity window that counts as a live turn")
		enterDelay = flag.Duration("enter-delay", 500*time.Millisecond, "pause between typing text and pressing Enter (prompt_toolkit ingest)")
		hermesHome = flag.String("hermes-home", defaultHermesHome(), "hermes home directory ($HERMES_HOME)")
		dbPath     = flag.String("db", "", "path to hermes state.db (default <hermes-home>/state.db)")
		captureMax = flag.Int("capture-max", 500, "max lines captured from a pane")
	)
	flag.Parse()

	switch *mode {
	case "local", "spagetti":
	default:
		log.Fatalf("mode %q: must be local or spagetti", *mode)
	}

	if *dbPath == "" {
		*dbPath = filepath.Join(*hermesHome, "state.db")
	}

	reg := NewRegistry(*hermesHome, *dbPath, *turnWindow, *captureMax)
	enterDelayMs = (*enterDelay).Milliseconds()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go reg.Run(ctx, *pollEvery)

	mux := reg.Routes()

	// spagetti mode: dial out to the gateway and serve the same API through an
	// end-to-end encrypted channel, so no inbound port is needed.
	if *mode == "spagetti" {
		log.Printf("tmux-control starting in spagetti mode (poll=%s hermes-home=%s db=%s env=%s)", *pollEvery, *hermesHome, *dbPath, *envFile)
		if err := runSpagetti(ctx, *envFile, mux); err != nil && !errors.Is(err, context.Canceled) {
			log.Fatalf("spagetti: %v", err)
		}
		log.Printf("stopped")
		return
	}

	srv := &http.Server{Addr: *listen, Handler: mux}

	go func() {
		<-ctx.Done()
		shCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(shCtx)
	}()

	log.Printf("tmux-control listening on http://%s (poll=%s hermes-home=%s db=%s)", *listen, *pollEvery, *hermesHome, *dbPath)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("http server: %v", err)
	}
	log.Printf("stopped")
}
