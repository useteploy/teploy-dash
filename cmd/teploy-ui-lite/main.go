package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/useteploy/teploy-ui/internal/server"
)

// teploy-ui-lite: dashboard only, no monitoring.
// This is the version that will replace the CLI's embedded UI.
func main() {
	port := flag.Int("port", 3456, "HTTP server port")
	host := flag.String("host", "0.0.0.0", "HTTP server host")
	deploymentsDir := flag.String("deployments", "/deployments", "CLI state files directory")
	flag.Parse()

	srv := server.New(server.Config{
		Host:           *host,
		Port:           *port,
		DeploymentsDir: *deploymentsDir,
		Monitor:        nil,
		Store:          nil,
	})

	go func() {
		addr := fmt.Sprintf("%s:%d", *host, *port)
		log.Printf("teploy-ui-lite listening on http://%s", addr)
		if err := srv.ListenAndServe(addr); err != nil {
			log.Fatalf("Server error: %v", err)
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	log.Println("Shutting down...")
}
