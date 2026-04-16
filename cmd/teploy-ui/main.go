package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/useteploy/teploy-ui/internal/alert"
	"github.com/useteploy/teploy-ui/internal/monitor"
	"github.com/useteploy/teploy-ui/internal/server"
	"github.com/useteploy/teploy-ui/internal/store"
)

func main() {
	port := flag.Int("port", 3456, "HTTP server port")
	host := flag.String("host", "0.0.0.0", "HTTP server host")
	dataDir := flag.String("data", "/var/teploy-ui", "Data directory for monitor history")
	deploymentsDir := flag.String("deployments", "/deployments", "CLI state files directory")
	nucleusURL := flag.String("nucleus-url", "", "Nucleus database URL (optional, uses JSONL files if not set)")
	flag.Parse()

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

	// Initialize monitor runner
	mon := monitor.New(st)

	// Initialize HTTP server
	srv := server.New(server.Config{
		Host:           *host,
		Port:           *port,
		DeploymentsDir: *deploymentsDir,
		Monitor:        mon,
		Store:          st,
	})

	// Load alert config and wire to monitors so state transitions fire notifications.
	notifCfg := server.LoadNotificationsConfig()
	if notifCfg.WebhookURL != "" || notifCfg.SMTPHost != "" {
		mon.SetAlerter(alert.New(notifCfg))
		log.Printf("Alerts configured")
	}

	// Start monitor checks
	mon.Start()

	// Start daily cleanup for file store (removes checks older than 30 days)
	var cleanupTicker *time.Ticker
	if fileStore != nil {
		cleanupTicker = time.NewTicker(24 * time.Hour)
		go func() {
			// Run cleanup once at startup
			if err := fileStore.Cleanup(); err != nil {
				log.Printf("Cleanup error: %v", err)
			}
			for range cleanupTicker.C {
				if err := fileStore.Cleanup(); err != nil {
					log.Printf("Cleanup error: %v", err)
				}
			}
		}()
	}

	// Start server
	go func() {
		addr := fmt.Sprintf("%s:%d", *host, *port)
		log.Printf("teploy-ui listening on http://%s", addr)
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
	st.Close()
}
