package main

import (
	"context"
	"embed"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"net"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/useteploy/teploy-dash/internal/alert"
	"github.com/useteploy/teploy-dash/internal/monitor"
	"github.com/useteploy/teploy-dash/internal/restoretest"
	"github.com/useteploy/teploy-dash/internal/server"
	"github.com/useteploy/teploy-dash/internal/store"
)

// frontendFS embeds the SPA shipped with the binary. Without this the
// binary breaks the moment it runs outside the source tree.
//
//go:embed frontend
var frontendFS embed.FS

// Set by goreleaser via -ldflags "-X main.version=..." (see .goreleaser.yaml,
// which has always passed these — the variables just didn't exist until now).
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

func main() {
	if err := run(); err != nil {
		log.Printf("teploy-dash stopped: %v", err)
		os.Exit(1)
	}
}

func run() error {
	port := flag.Int("port", 3456, "HTTP server port")
	host := flag.String("host", "0.0.0.0", "HTTP server host")
	dataDir := flag.String("data", "/var/teploy-dash", "Data directory for monitor history")
	deploymentsDir := flag.String("deployments", "/deployments", "CLI state files directory")
	nucleusURL := flag.String("nucleus-url", "", "Nucleus database URL (optional, uses JSONL files if not set)")
	noAuth := flag.Bool("no-auth", false, "disable HTTP Basic Auth (DANGEROUS — local dev only)")
	publicStatus := flag.Bool("public-status", false, "serve an unauthenticated /status page (monitor name + up/down + 24h uptime only)")
	flag.Parse()

	// Env fallback for the public status toggle (Docker-friendly).
	if v := os.Getenv("TEPLOY_DASH_PUBLIC_STATUS"); v == "1" || v == "true" {
		*publicStatus = true
	}

	// Auth: read bootstrap credentials from env. If neither TEPLOY_DASH_PASSWORD
	// nor a saved auth.json exist, the server starts in setup mode so the user
	// can create their account via the UI.
	authUser := os.Getenv("TEPLOY_DASH_USER")
	authPass := os.Getenv("TEPLOY_DASH_PASSWORD")
	if authUser == "" {
		authUser = "admin"
	}
	if *noAuth {
		// A63: no-auth is a local-development mode; binding it to a
		// non-loopback address publishes an unauthenticated deployment-control
		// surface. Refuse unless the operator takes the explicit opt-in env.
		if os.Getenv("TEPLOY_DASH_UNSAFE_NO_AUTH") != "1" {
			ip := net.ParseIP(*host)
			if ip == nil || !ip.IsLoopback() {
				return fmt.Errorf("--no-auth requires --host 127.0.0.1 or --host ::1 (set TEPLOY_DASH_UNSAFE_NO_AUTH=1 to override deliberately)")
			}
		}
		log.Println("WARNING: --no-auth enabled. UI is accessible without authentication.")
		authUser = ""
		authPass = ""
	}

	// A48: single-owner guard for the data directory. Local file stores and
	// in-memory schedulers have no cross-process coordination; a second
	// instance pointed at the same data would race rewrites and run duplicate
	// schedules. The lock is held for the process lifetime.
	release, lockErr := acquireInstanceLock(*dataDir)
	if lockErr != nil {
		return lockErr
	}
	defer release()

	// Initialize store (Nucleus if configured, JSONL fallback). The fallback is
	// silent by default — a Nucleus outage otherwise degrades persistence,
	// retention, and multi-replica consistency semantics while the process
	// still reports healthy. TEPLOY_DASH_REQUIRE_NUCLEUS opts into failing
	// startup instead, for deployments where that silent downgrade is worse
	// than not starting. In the default (non-strict) fallback case, the active
	// backend is surfaced through /api/health rather than only a startup log
	// line, so it stays visible after the log has scrolled away.
	requireNucleus := os.Getenv("TEPLOY_DASH_REQUIRE_NUCLEUS") == "1" || os.Getenv("TEPLOY_DASH_REQUIRE_NUCLEUS") == "true"
	if requireNucleus && *nucleusURL == "" {
		return fmt.Errorf("Nucleus is required (TEPLOY_DASH_REQUIRE_NUCLEUS) but no --nucleus-url was configured")
	}
	var st store.Store
	var fileStore *store.FileStore
	var err error
	backend := "file"
	if *nucleusURL != "" {
		st, err = store.NewNucleusStore(*nucleusURL)
		if err != nil {
			if requireNucleus {
				// Never log the URL: it can carry credentials in its DSN.
				return fmt.Errorf("Nucleus required (TEPLOY_DASH_REQUIRE_NUCLEUS) but the connection failed: %w", err)
			}
			// Same redaction on the fallback path.
			log.Printf("Warning: failed to connect to Nucleus, falling back to file store: %v", err)
			fileStore = store.NewFileStore(*dataDir)
			// A47: the fallback backend gets the same initialization check as
			// the direct file path — a data directory that cannot back a
			// store must fail startup, not surface later per-operation.
			if ferr := fileStore.InitErr(); ferr != nil {
				return fmt.Errorf("file store unavailable in %s (Nucleus fallback): %w", *dataDir, ferr)
			}
			st = fileStore
		} else {
			backend = "nucleus"
		}
	} else {
		fileStore = store.NewFileStore(*dataDir)
		if err := fileStore.InitErr(); err != nil {
			log.Fatalf("file store unavailable in %s: %v", *dataDir, err)
		}
		st = fileStore
	}

	// Initialize monitor + restore-test runners
	mon := monitor.New(st)
	rst := restoretest.New(st)

	// Strip the embed prefix so the FS is rooted at the frontend/ contents
	// (index.html sits at "/", css/ and js/ at the expected URL paths).
	uiFS, err := fs.Sub(frontendFS, "frontend")
	if err != nil {
		log.Fatalf("embed frontend: %v", err)
	}

	// Initialize HTTP server
	srv := server.New(server.Config{
		Host:           *host,
		Port:           *port,
		DeploymentsDir: *deploymentsDir,
		DataDir:        *dataDir,
		Monitor:        mon,
		Restore:        rst,
		Store:          st,
		AuthUser:       authUser,
		AuthPass:       authPass,
		NoAuth:         *noAuth,
		PublicStatus:   *publicStatus,
		Frontend:       uiFS,
		Version:        version,
		Backend:        backend,
	})

	// Load alert config and wire to monitors so state transitions fire notifications.
	notifCfg := server.LoadNotificationsConfig()
	if notifCfg.WebhookURL != "" || notifCfg.SMTPHost != "" {
		mon.SetAlerter(alert.New(notifCfg))
		rst.SetAlerter(alert.New(notifCfg))
		log.Printf("Alerts configured")
	}

	// Start monitor checks + restore-test schedules
	mon.Start()
	rst.Start()

	// Start daily cleanup for file store (removes checks older than store.RetentionDays)
	// Runs for ANY backend now (previously fileStore-only, so the Nucleus
	// checks table grew unbounded). Tied to cleanupCtx + cleanupWG so shutdown
	// can cancel and join it: Ticker.Stop() alone doesn't close the channel,
	// so a bare `for range ticker.C` blocks forever and the goroutine (and
	// anything it touches on the store) can outlive st.Close().
	cleanupCtx, cleanupCancel := context.WithCancel(context.Background())
	var cleanupWG sync.WaitGroup
	cleanupTicker := time.NewTicker(24 * time.Hour)
	cleanupWG.Add(1)
	go func() {
		defer cleanupWG.Done()
		defer cleanupTicker.Stop()
		if err := st.Cleanup(); err != nil {
			log.Printf("Cleanup error: %v", err)
		}
		for {
			select {
			case <-cleanupCtx.Done():
				return
			case <-cleanupTicker.C:
				if err := st.Cleanup(); err != nil {
					log.Printf("Cleanup error: %v", err)
				}
			}
		}
	}()

	// Start server
	serverErrCh := make(chan error, 1)
	go func() {
		addr := net.JoinHostPort(*host, strconv.Itoa(*port))
		log.Printf("teploy-dash listening on http://%s", addr)
		if err := srv.ListenAndServe(addr); err != nil {
			serverErrCh <- err
		}
	}()

	// Graceful shutdown: stop accepting HTTP traffic and drain in-flight
	// requests FIRST, then stop background workers, then close storage last —
	// so no in-flight handler or worker can touch a closed store.
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	select {
	case <-quit:
		log.Println("Shutting down...")
	case err := <-serverErrCh:
		// A failed listener (port in use, bad bind address) is a startup
		// error and must exit non-zero, not just log and return 0 (A39).
		cleanupCancel()
		return fmt.Errorf("HTTP server failed: %w", err)
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("HTTP shutdown error: %v", err)
	}

	cleanupCancel()
	cleanupDone := make(chan struct{})
	go func() {
		cleanupWG.Wait()
		close(cleanupDone)
	}()
	select {
	case <-cleanupDone:
	case <-time.After(5 * time.Second):
		log.Println("cleanup worker did not stop in time")
	}

	mon.Stop()
	rst.Stop()
	st.Close()
	return nil
}
