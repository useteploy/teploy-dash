package main

import (
	"context"
	"embed"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"math"
	"net"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/useteploy/teploy-dash/internal/alert"
	"github.com/useteploy/teploy-dash/internal/monitor"
	"github.com/useteploy/teploy-dash/internal/outbox"
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

// envInt64 reads a non-negative integer env var; unparsable or negative
// values log a warning and fall back to def.
func envInt64(name string, def int64) int64 {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return def
	}
	v, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || v < 0 {
		log.Printf("Warning: ignoring invalid %s=%q", name, raw)
		return def
	}
	return v
}

// envInt bounds an env count to the platform int range (R62): an envInt64
// value above MaxInt32 would silently wrap on 32-bit targets and overflow
// int on any target near the limit.
func envInt(name string, def int) int {
	v := envInt64(name, int64(def))
	if v > math.MaxInt32 {
		log.Printf("Warning: %s=%d exceeds the maximum; using %d", name, v, math.MaxInt32)
		return math.MaxInt32
	}
	return int(v)
}

// durationFromDays converts whole days to a Duration with an explicit
// overflow check (R62): an unbounded positive value used to wrap NEGATIVE,
// and the operation manager interprets negative age as "retention
// disabled" — an invalid setting silently became a dangerous one.
func durationFromDays(name string, days int64) (time.Duration, error) {
	const day = 24 * time.Hour
	const maxDays = int64(math.MaxInt64) / int64(day)
	if days < 0 || days > maxDays {
		return 0, fmt.Errorf("%s must be between 0 and %d days", name, maxDays)
	}
	return time.Duration(days) * day, nil
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

	// Operation history retention (useteploy__teploy-dash-04). Defaults live
	// in the operation package; these envs override for chatty installs.
	opJournalBytes := envInt64("TEPLOY_DASH_OPERATION_JOURNAL_BYTES", 0)
	opHistoryDays := envInt64("TEPLOY_DASH_OPERATION_HISTORY_DAYS", 0)
	opMaxOperations := envInt("TEPLOY_DASH_MAX_OPERATIONS", 0)
	// Per-target admission budget (A12/A27): how many non-terminal operations
	// may be queued for one server before further enqueues are rejected.
	opMaxQueued := envInt("TEPLOY_DASH_MAX_QUEUED_PER_TARGET", 0)
	// Global admission + execution bounds (R17): total live operations and
	// simultaneous CLI executions across every target.
	opMaxLive := envInt("TEPLOY_DASH_MAX_LIVE_OPERATIONS", 0)
	opMaxConcurrent := envInt("TEPLOY_DASH_MAX_CONCURRENT_OPERATIONS", 0)
	// R62: reject an unrepresentable retention age at startup instead of
	// wrapping it negative (which disables retention).
	opHistoryAge, historyErr := durationFromDays("TEPLOY_DASH_OPERATION_HISTORY_DAYS", opHistoryDays)
	if historyErr != nil {
		return historyErr
	}
	// D02 idempotency window: how long an admitted Idempotency-Key is
	// honored, per principal (0/unset = 24h default; a negative duration
	// disables expiry).
	opIdempotencyWindow := time.Duration(0)
	if raw := strings.TrimSpace(os.Getenv("TEPLOY_DASH_IDEMPOTENCY_WINDOW")); raw != "" {
		parsed, err := time.ParseDuration(raw)
		if err != nil || parsed < 0 {
			return fmt.Errorf("TEPLOY_DASH_IDEMPOTENCY_WINDOW must be a non-negative duration (e.g. 24h), got %q", raw)
		}
		opIdempotencyWindow = parsed
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
			// R39: return the error instead of log.Fatalf, so the deferred
			// instance-lock release (and any future cleanup) still runs.
			return fmt.Errorf("file store unavailable in %s: %w", *dataDir, err)
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
		st.Close()
		return fmt.Errorf("embed frontend: %w", err)
	}

	// R39: BIND the listener before any background service starts. A failed
	// bind (port in use, bad address) previously raced monitors, restore
	// schedules, and a possibly-running restore CLI child against an early
	// return that skipped the normal cleanup sequence entirely.
	addr := net.JoinHostPort(*host, strconv.Itoa(*port))
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		st.Close()
		return fmt.Errorf("bind HTTP listener %s: %w", addr, err)
	}

	// Initialize HTTP server
	srv := server.New(server.Config{
		Host:                       *host,
		Port:                       *port,
		DeploymentsDir:             *deploymentsDir,
		DataDir:                    *dataDir,
		Monitor:                    mon,
		Restore:                    rst,
		Store:                      st,
		AuthUser:                   authUser,
		AuthPass:                   authPass,
		NoAuth:                     *noAuth,
		PublicStatus:               *publicStatus,
		Frontend:                   uiFS,
		Version:                    version,
		Backend:                    backend,
		OperationMaxJournalBytes:   opJournalBytes,
		OperationMaxHistoryAge:     opHistoryAge,
		OperationMaxOperations:     opMaxOperations,
		OperationMaxQueued:         opMaxQueued,
		OperationMaxLive:           opMaxLive,
		OperationMaxConcurrent:     opMaxConcurrent,
		OperationIdempotencyWindow: opIdempotencyWindow,
	})

	// D08: durable alert delivery. The outbox journals every monitor-alert
	// attempt in the data dir, retries with backoff, dead-letters after a
	// bounded count, surfaces the last failure on the monitor API, and
	// resumes pending deliveries after a restart without re-sending
	// delivered ones. An unwritable journal is a broken persistence
	// contract — refuse to start rather than silently degrade to
	// best-effort (A32 discipline).
	alertOutbox, err := outbox.New(*dataDir, outbox.Options{
		ConfigFn: server.LoadNotificationsConfig,
	})
	if err != nil {
		st.Close()
		return fmt.Errorf("alert outbox: %w", err)
	}
	mon.SetAlerter(alertOutbox)

	// Load alert config for the restore runner (which keeps the direct
	// dispatcher; routing it through the outbox is recorded D08 remainder).
	// R14: an unreadable/corrupt config is loud at startup — it previously
	// read as "not configured" and a later partial save destroyed secrets.
	notifCfg, notifErr := server.LoadNotificationsConfig()
	if notifErr != nil {
		log.Printf("Warning: %v (alerts disabled until the config is repaired; saving from Settings is refused in this state)", notifErr)
	} else if notifCfg.WebhookURL != "" || notifCfg.SMTPHost != "" {
		rst.SetAlerter(alert.New(notifCfg))
		log.Printf("Alerts configured")
	}

	// Start monitor checks + restore-test schedules, then the alert outbox
	// worker (pending deliveries resume from the journal).
	mon.Start()
	rst.Start()
	alertOutbox.Start()

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

	// Start the HTTP server on the pre-bound listener. A LATER failure
	// (accept error, TLS misconfiguration) funnels through the same shutdown
	// epilogue as a signal — it no longer bypasses runner joins and the
	// store close (R39).
	serverErrCh := make(chan error, 1)
	go func() {
		log.Printf("teploy-dash listening on http://%s", addr)
		if err := srv.Serve(listener); err != nil {
			serverErrCh <- err
		}
	}()

	// Graceful shutdown: stop accepting HTTP traffic and drain in-flight
	// requests FIRST, then stop background workers, then close storage last —
	// so no in-flight handler or worker can touch a closed store. R39: both
	// exit paths (signal, listener failure) share this one epilogue.
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	var runErr error
	select {
	case <-quit:
		log.Println("Shutting down...")
	case runErr = <-serverErrCh:
		log.Printf("HTTP server failed: %v", runErr)
	}
	signal.Stop(quit)

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		// F022: a timed-out drain must not just log and continue as though
		// every handler ended — close the remaining connections so a
		// straggler cannot keep mutating (or admitting work) while the rest
		// of the shutdown sequence proceeds.
		log.Printf("HTTP shutdown error (closing remaining connections): %v", err)
		srv.CloseHTTP()
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

	// F023: ONE shared worker budget (monitor join + restore join + operation
	// drain) instead of each phase carrying its own independent ceiling —
	// the previous worst case (15+5+90+90+15s) far exceeded the supervisor's
	// stop budget, so systemd SIGKILLed the process mid-drain. The installer
	// unit now allows 180s against this ~140s worst case.
	workerCtx, workerCancel := context.WithTimeout(context.Background(), 120*time.Second)
	mon.Stop(workerCtx)
	rst.Stop(workerCtx)
	// D08: join in-flight alert-delivery attempts before the store closes
	// (bounded by the same shared budget; a cut-short attempt is re-sent on
	// next start — at-least-once).
	alertOutbox.Stop(workerCtx)
	// A39/A47: join in-flight operation work (bounded by the REMAINING
	// shared budget) before the store closes — a hard exit used to kill CLI
	// children mid-write and leave records for recovery to mark interrupted.
	srv.DrainOperations(workerCtx)
	workerCancel()
	st.Close()
	return runErr
}
