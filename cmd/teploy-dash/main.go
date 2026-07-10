package main

import (
	"embed"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"os"
	"os/signal"
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

func main() {
	port := flag.Int("port", 3456, "HTTP server port")
	host := flag.String("host", "0.0.0.0", "HTTP server host")
	dataDir := flag.String("data", "/var/teploy-dash", "Data directory for monitor history")
	deploymentsDir := flag.String("deployments", "/deployments", "CLI state files directory")
	nucleusURL := flag.String("nucleus-url", "", "Nucleus database URL (optional, uses JSONL files if not set)")
	noAuth := flag.Bool("no-auth", false, "disable HTTP Basic Auth (DANGEROUS — local dev only)")
	flag.Parse()

	// Auth: read bootstrap credentials from env. If neither TEPLOY_DASH_PASSWORD
	// nor a saved auth.json exist, the server starts in setup mode so the user
	// can create their account via the UI.
	authUser := os.Getenv("TEPLOY_DASH_USER")
	authPass := os.Getenv("TEPLOY_DASH_PASSWORD")
	if authUser == "" {
		authUser = "admin"
	}
	if *noAuth {
		log.Println("WARNING: --no-auth enabled. UI is accessible without authentication.")
		authUser = ""
		authPass = ""
	}

	// Initialize store (Nucleus if configured, JSONL fallback)
	var st store.Store
	var fileStore *store.FileStore
	var err error
	if *nucleusURL != "" {
		st, err = store.NewNucleusStore(*nucleusURL)
		if err != nil {
			log.Printf("Warning: failed to connect to Nucleus (%s), falling back to file store: %v", *nucleusURL, err)
			fileStore = store.NewFileStore(*dataDir)
			st = fileStore
		}
	} else {
		fileStore = store.NewFileStore(*dataDir)
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
		Frontend:       uiFS,
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
	// checks table grew unbounded).
	cleanupTicker := time.NewTicker(24 * time.Hour)
	go func() {
		if err := st.Cleanup(); err != nil {
			log.Printf("Cleanup error: %v", err)
		}
		for range cleanupTicker.C {
			if err := st.Cleanup(); err != nil {
				log.Printf("Cleanup error: %v", err)
			}
		}
	}()

	// Start server
	go func() {
		addr := fmt.Sprintf("%s:%d", *host, *port)
		log.Printf("teploy-dash listening on http://%s", addr)
		if err := srv.ListenAndServe(addr); err != nil {
			log.Fatalf("Server error: %v", err)
		}
	}()

	// Graceful shutdown
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	log.Println("Shutting down...")
	if cleanupTicker != nil {
		cleanupTicker.Stop()
	}
	mon.Stop()
	rst.Stop()
	st.Close()
}
