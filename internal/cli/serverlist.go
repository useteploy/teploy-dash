package cli

// Server-list decode across the MI 2 transition (X02 S2 tail, corpus rev
// 4). `server list --json` had no envelope object before MI 2 — it emitted
// a bare map-of-servers at the root, which is why adopting the new
// {machine_interface, servers[], observed_at} envelope is a contract bump
// and not a field add. Dash runs against whatever teploy is on PATH, so
// the decode must not break against EITHER a pre-reshape CLI (bare map)
// or a post-reshape CLI (envelope) — and must refuse, centrally, an
// envelope from an interface newer than MaxSupportedMachineInterface.

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sort"
	"sync"
	"time"
)

// ServerRecord is one registered server as `server list --json` reports
// it, normalized across both wire eras.
type ServerRecord struct {
	Name string
	ID   string
	Host string
	User string
	Role string
}

// DecodeServerList decodes `teploy server list --json` stdout in both
// eras: the MI-2 envelope {machine_interface, servers[], observed_at},
// or the legacy bare map-of-servers (pre-MI-2 CLIs; MI-1 CLIs too — the
// command had no envelope before MI 2). The decode rides ParseJSON, so
// the central machine-interface gate refuses an envelope newer than
// MaxSupportedMachineInterface (naming the upgrade remedy) before any
// entry is interpreted. Records return sorted by name.
//
// Discrimination is by the machine_interface key: only the real envelope
// carries it (a legacy bare map cannot — a server record is always an
// object, and the phantom-key hazard is exactly why the CLI reshaped).
// An envelope-shape payload that fails strict decode is an ERROR, never
// a legacy fallback: an MI-bearing producer speaks the envelope contract.
func DecodeServerList(raw string) ([]ServerRecord, error) {
	data, err := ParseJSON(raw)
	if err != nil {
		return nil, err
	}
	if m, ok := data.(map[string]interface{}); ok {
		if _, isEnvelope := m["machine_interface"]; isEnvelope {
			return decodeServerListEnvelope(raw)
		}
	}
	return decodeServerListLegacy(raw)
}

func decodeServerListEnvelope(raw string) ([]ServerRecord, error) {
	var envelope struct {
		MachineInterface int `json:"machine_interface"`
		Servers          []struct {
			Name string `json:"name"`
			ID   string `json:"id"`
			Host string `json:"host"`
			User string `json:"user"`
			Role string `json:"role"`
		} `json:"servers"`
		ObservedAt time.Time `json:"observed_at"`
	}
	if err := json.Unmarshal([]byte(raw), &envelope); err != nil {
		return nil, fmt.Errorf("malformed server list envelope: %w", err)
	}
	if envelope.Servers == nil {
		return nil, errors.New("malformed server list envelope: servers must be an array")
	}
	records := make([]ServerRecord, 0, len(envelope.Servers))
	for _, srv := range envelope.Servers {
		if srv.Name == "" || srv.Host == "" {
			return nil, fmt.Errorf("malformed server list envelope: entry %+v lacks name or host", srv)
		}
		records = append(records, ServerRecord{Name: srv.Name, ID: srv.ID, Host: srv.Host, User: srv.User, Role: srv.Role})
	}
	sort.Slice(records, func(i, j int) bool { return records[i].Name < records[j].Name })
	return records, nil
}

// legacyServerListDeprecation logs the legacy decode ONCE per process:
// every fleet sweep and registry read hits this path against a pre-reshape
// CLI; one line names the skew, the rest stay silent.
var legacyServerListDeprecation sync.Once

func decodeServerListLegacy(raw string) ([]ServerRecord, error) {
	var legacy map[string]struct {
		ID   string `json:"id"`
		Host string `json:"host"`
		User string `json:"user"`
		Role string `json:"role"`
	}
	if err := json.Unmarshal([]byte(raw), &legacy); err != nil {
		return nil, fmt.Errorf("parsing the server list: %w", err)
	}
	legacyServerListDeprecation.Do(func() {
		log.Printf("[cli] server list: legacy bare-map shape from a pre-MI-2 teploy CLI; decoding via the legacy path — upgrade the CLI to adopt the machine envelope")
	})
	records := make([]ServerRecord, 0, len(legacy))
	for name, srv := range legacy {
		records = append(records, ServerRecord{Name: name, ID: srv.ID, Host: srv.Host, User: srv.User, Role: srv.Role})
	}
	sort.Slice(records, func(i, j int) bool { return records[i].Name < records[j].Name })
	return records, nil
}
