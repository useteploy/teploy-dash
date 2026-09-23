package server

import (
	"context"
	"encoding/json"
	"fmt"
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
	// MachineInterface is the CLI's advertised machine interface (X02
	// S1/S2): 0 means a pre-MI CLI (the --json handshake field was absent
	// or the probe fell back to plain `version`).
	MachineInterface int `json:"machine_interface"`
	// CapabilityTokens are the tokens the CLI advertised in its version
	// handshake; empty for pre-MI CLIs (the --help probes ran instead).
	CapabilityTokens []string `json:"capability_tokens,omitempty"`
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
		// X02 S1/S2: source the contract from `version --json` when the CLI
		// speaks the machine interface, and FAIL CLOSED on an interface
		// newer than the one this dash decodes. Pre-MI CLIs do not know
		// --json on version; that failure falls back to the legacy probes.
		if result, err := s.runCLI(ctx, "version", "--json"); err != nil {
			value.Errors = append(value.Errors, capabilityError{Probe: "version", Message: err.Error()})
		} else if result.ExitCode == 0 {
			// Inspection decode, deliberately NOT cli.ParseJSON: the central
			// decode refuses newer-machine-interface envelopes (correct for
			// data), but this probe's whole job is to READ a newer
			// handshake so the failure names the remedy. Nothing from a
			// too-new CLI is interpreted beyond this struct.
			var handshake struct {
				Version          string   `json:"version"`
				MachineInterface int      `json:"machine_interface"`
				CapabilityTokens []string `json:"capabilities"`
			}
			dec := json.NewDecoder(strings.NewReader(strings.TrimSpace(result.Stdout)))
			dec.UseNumber()
			if derr := dec.Decode(&handshake); derr != nil {
				value.Errors = append(value.Errors, capabilityError{Probe: "version", Message: derr.Error()})
			} else {
				value.CLI.Version = normalizeCLIVersion(handshake.Version)
				value.CLI.MachineInterface = handshake.MachineInterface
				value.CLI.CapabilityTokens = handshake.CapabilityTokens
				if value.CLI.MachineInterface > cli.MaxSupportedMachineInterface {
					// Fail closed BEFORE any envelope or mutation rides the
					// newer interface: machine features report unsupported
					// with the remedy visible at /api/capabilities.
					value.Errors = append(value.Errors, capabilityError{
						Probe:   "version",
						Message: fmt.Sprintf("CLI machine interface %d is newer than this dash supports (max %d) — upgrade teploy-dash", value.CLI.MachineInterface, cli.MaxSupportedMachineInterface),
					})
					s.capabilitiesCache.value = value
					s.capabilitiesCache.builtAt = now
					return value
				}
				hasToken := func(t string) bool {
					for _, x := range value.CLI.CapabilityTokens {
						if x == t {
							return true
						}
					}
					return false
				}
				value.Features.AppListJSON = hasToken("app-list-machine")
				value.Features.ServerStatusJSON = hasToken("server-status-machine")
			}
		} else {
			// Pre-MI CLI (or version --json refused): the legacy path.
			if result, err := s.runCLI(ctx, "version"); err != nil {
				value.Errors = append(value.Errors, capabilityError{Probe: "version", Message: err.Error()})
			} else if result.ExitCode != 0 {
				value.Errors = append(value.Errors, capabilityError{Probe: "version", Message: commandFailure([]string{"version"}, result).Error()})
			} else {
				value.CLI.Version = normalizeCLIVersion(result.Stdout)
			}
		}
		// --help probes only run when the token set did not already answer
		// (pre-MI CLIs); a token-bearing MI CLI does not need them.
		if len(value.CLI.CapabilityTokens) == 0 {
			value.Features.AppListJSON = value.Features.AppListJSON || s.probeCommand(ctx, &value, "app_list_json", "app", "list", "--help")
			value.Features.ServerStatusJSON = value.Features.ServerStatusJSON || s.probeCommand(ctx, &value, "server_status_json", "server", "status", "--help")
		}
	}

	s.capabilitiesCache.value = value
	s.capabilitiesCache.builtAt = now
	return value
}

// probeCommand reports whether the command's help output advertises the
// machine --json flag (F072): a zero help exit alone only proved the
// command EXISTS — an older CLI without --json was advertised as
// supporting machine output, and the later fallback had to discover the
// truth per call. A non-zero exit that is not a recognizable
// "unsupported command" is surfaced as a probe error rather than a clean
// "unsupported".
func (s *Server) probeCommand(ctx context.Context, value *capabilities, name string, args ...string) bool {
	result, err := s.runCLI(ctx, args...)
	if err != nil {
		value.Errors = append(value.Errors, capabilityError{Probe: name, Message: err.Error()})
		return false
	}
	if result.ExitCode == 0 {
		return strings.Contains(result.Stdout, "--json") || strings.Contains(result.Stderr, "--json")
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
