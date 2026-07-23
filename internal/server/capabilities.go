package server

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/useteploy/teploy-dash/internal/cli"
)

const capabilityCacheTTL = 5 * time.Minute

type capabilityCache struct {
	mu      sync.Mutex
	value   capabilities
	builtAt time.Time
}

type capabilities struct {
	CLI      cliCapability     `json:"cli"`
	Features machineFeatures   `json:"features"`
	ProbedAt time.Time         `json:"probed_at"`
	Errors   []capabilityError `json:"errors"`
}

type cliCapability struct {
	Installed bool   `json:"installed"`
	Version   string `json:"version"`
}

type machineFeatures struct {
	AppListJSON      bool `json:"app_list_json"`
	ServerStatusJSON bool `json:"server_status_json"`
	Operations       bool `json:"operations"`
}

type capabilityError struct {
	Probe   string `json:"probe"`
	Message string `json:"message"`
}

func (s *Server) capabilities(ctx context.Context) capabilities {
	s.capabilitiesCache.mu.Lock()
	defer s.capabilitiesCache.mu.Unlock()
	if !s.capabilitiesCache.builtAt.IsZero() && time.Since(s.capabilitiesCache.builtAt) < capabilityCacheTTL {
		return s.capabilitiesCache.value
	}

	now := time.Now().UTC()
	value := capabilities{
		CLI: cliCapability{Installed: s.cliInstalled()},
		Features: machineFeatures{
			Operations: s.operations != nil,
		},
		ProbedAt: now,
		Errors:   []capabilityError{},
	}
	if value.CLI.Installed {
		if result, err := s.runCLI(ctx, "version"); err != nil {
			value.Errors = append(value.Errors, capabilityError{Probe: "version", Message: err.Error()})
		} else if result.ExitCode != 0 {
			value.Errors = append(value.Errors, capabilityError{Probe: "version", Message: commandFailure([]string{"version"}, result).Error()})
		} else {
			value.CLI.Version = normalizeCLIVersion(result.Stdout)
		}
		value.Features.AppListJSON = s.probeCommand(ctx, &value, "app_list_json", "app", "list", "--help")
		value.Features.ServerStatusJSON = s.probeCommand(ctx, &value, "server_status_json", "server", "status", "--help")
	}

	s.capabilitiesCache.value = value
	s.capabilitiesCache.builtAt = now
	return value
}

func (s *Server) probeCommand(ctx context.Context, value *capabilities, name string, args ...string) bool {
	result, err := s.runCLI(ctx, args...)
	if err != nil {
		value.Errors = append(value.Errors, capabilityError{Probe: name, Message: err.Error()})
		return false
	}
	if result.ExitCode == 0 {
		return true
	}
	if !cli.UnsupportedCommand(result) {
		value.Errors = append(value.Errors, capabilityError{Probe: name, Message: commandFailure(args, result).Error()})
	}
	return false
}

func normalizeCLIVersion(value string) string {
	value = strings.TrimSpace(value)
	value = strings.TrimPrefix(value, "teploy ")
	return strings.TrimSpace(value)
}

func (s *Server) handleCapabilities(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	writeData(w, s.capabilities(r.Context()))
}
