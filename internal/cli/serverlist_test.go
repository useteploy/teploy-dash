package cli

import (
	"strings"
	"testing"
)

// X02 S2 tail (corpus rev 4): `server list --json` decode across the MI 2
// transition. Dash must not break against EITHER a pre-reshape CLI (bare
// map) or a post-reshape CLI (envelope), and must refuse an envelope from
// a newer interface through the central gate.

const serverListEnvelopeMI2 = `{
  "machine_interface": 2,
  "servers": [
    {"name": "prod", "id": "srv-0123456789abcdef", "host": "192.0.2.10", "user": "deploy", "role": "app"},
    {"name": "staging", "host": "192.0.2.20"}
  ],
  "observed_at": "2026-09-23T12:00:00Z"
}`

const serverListLegacyBareMap = `{
  "prod": {"id": "srv-0123456789abcdef", "host": "192.0.2.10", "user": "deploy", "role": "app"},
  "staging": {"host": "192.0.2.20"}
}`

func TestDecodeServerListEnvelopeMI2(t *testing.T) {
	records, err := DecodeServerList(serverListEnvelopeMI2)
	if err != nil {
		t.Fatalf("MI 2 envelope must decode: %v", err)
	}
	if len(records) != 2 {
		t.Fatalf("records = %#v", records)
	}
	if records[0].Name != "prod" || records[0].ID != "srv-0123456789abcdef" ||
		records[0].Host != "192.0.2.10" || records[0].User != "deploy" || records[0].Role != "app" {
		t.Fatalf("prod record not mapped: %#v", records[0])
	}
	if records[1].Name != "staging" || records[1].ID != "" || records[1].Host != "192.0.2.20" {
		t.Fatalf("id-less legacy entry not mapped: %#v", records[1])
	}
	if records[0].Name > records[1].Name {
		t.Fatalf("records not sorted by name: %#v", records)
	}
}

func TestDecodeServerListLegacyBareMap(t *testing.T) {
	records, err := DecodeServerList(serverListLegacyBareMap)
	if err != nil {
		t.Fatalf("legacy bare map must decode (pre-reshape CLI on PATH): %v", err)
	}
	if len(records) != 2 || records[0].Name != "prod" || records[1].Name != "staging" {
		t.Fatalf("records = %#v", records)
	}
	if records[0].ID != "srv-0123456789abcdef" || records[0].User != "deploy" {
		t.Fatalf("legacy fields not mapped: %#v", records[0])
	}
}

func TestDecodeServerListEmptyFleetBothEras(t *testing.T) {
	records, err := DecodeServerList(`{"machine_interface": 2, "servers": [], "observed_at": "2026-09-23T12:00:00Z"}`)
	if err != nil || len(records) != 0 {
		t.Fatalf("empty envelope must decode to zero records: %#v %v", records, err)
	}
	records, err = DecodeServerList(`{}`)
	if err != nil || len(records) != 0 {
		t.Fatalf("empty bare map must decode to zero records: %#v %v", records, err)
	}
}

func TestDecodeServerListRefusesNewerInterfaceWithRemedy(t *testing.T) {
	_, err := DecodeServerList(`{"machine_interface": 3, "servers": [], "observed_at": "2026-09-23T12:00:00Z"}`)
	if err == nil {
		t.Fatal("an envelope newer than MaxSupportedMachineInterface must refuse")
	}
	if !strings.Contains(err.Error(), "newer than this dash supports") || !strings.Contains(err.Error(), "upgrade teploy-dash") {
		t.Fatalf("refusal must name the remedy, got %v", err)
	}
}

func TestDecodeServerListMalformedEnvelopeRefuses(t *testing.T) {
	// An MI-bearing producer speaks the envelope contract: a bad envelope
	// is an error, never a legacy fallback.
	for name, raw := range map[string]string{
		"machine_interface not a number": `{"machine_interface": "two", "servers": []}`,
		"servers not an array":           `{"machine_interface": 2, "servers": {"prod": {"host": "h"}}, "observed_at": "2026-09-23T12:00:00Z"}`,
		"entry missing name":             `{"machine_interface": 2, "servers": [{"host": "h"}], "observed_at": "2026-09-23T12:00:00Z"}`,
		"entry missing host":             `{"machine_interface": 2, "servers": [{"name": "prod"}], "observed_at": "2026-09-23T12:00:00Z"}`,
	} {
		if _, err := DecodeServerList(raw); err == nil {
			t.Fatalf("%s: malformed envelope must refuse", name)
		}
	}
}

func TestDecodeServerListNonObjectRefuses(t *testing.T) {
	if _, err := DecodeServerList(`["prod"]`); err == nil {
		t.Fatal("a non-object payload must refuse (it is neither era's shape)")
	}
	if _, err := DecodeServerList(""); err == nil {
		t.Fatal("empty output must refuse")
	}
}
