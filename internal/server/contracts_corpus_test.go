package server

// X01 5.2 job 3 / X02 ADR 4 ("CI consumption"): dash's legs of decoding the
// pinned contracts corpus. The node leg (ci/validate-contracts.mjs)
// schema-validates every valid/legacy fixture and asserts invalid/ ones
// fail; THIS file runs the fixtures through dash's real decode paths:
//
//   - version-handshake and app-list envelopes go through cli.ParseJSON
//     (the shared MI gate: missing machine_interface = pre-MI legacy
//     producer, never MI 0; newer-than-max refuses);
//   - the app-list envelope additionally goes through decodeAppListEnvelope
//     (the fleet read's completeness rules);
//   - the preview-state ambiguous fixture exercises the adoption-refusal
//     contract: dash must never mint a canonical identity for an ambiguous
//     legacy record (C06). Dash has no preview-state importer today; this
//     test is the seam that holds the rule until one lands (AUDIT_OPEN).
//
// Corpus location: TEPLOY_CONTRACTS_DIR when set (CI: the pinned teploy-cli
// checkout -- a missing or unreadable directory then FAILS, it does not
// skip); otherwise the sibling ../teploy-cli/contracts checkout, skipping
// with instructions when absent (same dev-ergonomics pattern as
// observation_contracts_test.go).

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/useteploy/teploy-dash/internal/cli"
)

func contractsCorpusDir(t *testing.T) string {
	t.Helper()
	if dir := os.Getenv("TEPLOY_CONTRACTS_DIR"); dir != "" {
		if _, err := os.Stat(dir); err != nil {
			t.Fatalf("TEPLOY_CONTRACTS_DIR=%s is not readable: %v (CI must fail, not skip, on a broken corpus checkout)", dir, err)
		}
		return dir
	}
	candidate := "../../../teploy-cli/contracts"
	if _, err := os.Stat(candidate); err != nil {
		t.Skipf("contracts corpus not found at %s (set TEPLOY_CONTRACTS_DIR or check out teploy-cli beside teploy-dash)", candidate)
	}
	return candidate
}

func readCorpusFixture(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(contractsCorpusDir(t), "fixtures", path))
	if err != nil {
		t.Fatalf("reading corpus fixture %s: %v", path, err)
	}
	return string(raw)
}

func TestContractsCorpusVersionHandshakeDecodes(t *testing.T) {
	raw := readCorpusFixture(t, "version-handshake/valid/mi2.json")
	data, err := cli.ParseJSON(raw)
	if err != nil {
		t.Fatalf("version handshake fixture must decode through dash's CLI JSON gate: %v", err)
	}
	m, ok := data.(map[string]interface{})
	if !ok {
		t.Fatalf("handshake fixture decoded to %T, want object", data)
	}
	if _, ok := m["machine_interface"]; !ok {
		t.Fatal("handshake fixture lost its machine_interface field in decode")
	}
	if caps, ok := m["capabilities"].([]interface{}); !ok || len(caps) == 0 {
		t.Fatalf("handshake fixture capabilities = %#v, want a non-empty array", m["capabilities"])
	}
}

func TestContractsCorpusAppListEnvelopeDecodes(t *testing.T) {
	raw := readCorpusFixture(t, "app-list-envelope/valid/mi2.json")
	if _, err := cli.ParseJSON(raw); err != nil {
		t.Fatalf("app-list fixture must decode through dash's CLI JSON gate: %v", err)
	}
	var list machineAppList
	if err := decodeAppListEnvelope(raw, "corpus", &list); err != nil {
		t.Fatalf("app-list fixture must pass the fleet decode completeness rules: %v", err)
	}
	if len(list.Apps) != 1 || list.Apps[0].App != "myapp" {
		t.Fatalf("app-list fixture apps = %#v", list.Apps)
	}
	if err := assertAppStatusComplete(list.Apps[0], "corpus"); err != nil {
		t.Fatalf("app-list fixture app must pass per-app completeness: %v", err)
	}
}

// The pre-MI envelope is a first-class corpus class (MANIFEST: legacy
// fixtures are states real deployments carry). Dash's contract: a missing
// machine_interface field means "pre-MI producer" -- the legacy decode
// path -- and is never interpreted as MI 0 or refused as newer.
func TestContractsCorpusAppListLegacyPreMIDecodesAsLegacy(t *testing.T) {
	raw := readCorpusFixture(t, "app-list-envelope/legacy/pre-mi.json")
	data, err := cli.ParseJSON(raw)
	if err != nil {
		t.Fatalf("pre-MI envelope must decode (legacy path), got: %v", err)
	}
	m, ok := data.(map[string]interface{})
	if !ok {
		t.Fatalf("pre-MI fixture decoded to %T, want object", data)
	}
	if _, present := m["machine_interface"]; present {
		t.Fatal("pre-MI fixture unexpectedly carries machine_interface; the corpus class lost its meaning")
	}
	var list machineAppList
	if err := decodeAppListEnvelope(raw, "corpus", &list); err != nil {
		t.Fatalf("pre-MI envelope must pass the same machine decode rules (the pre-MI CLI populated every collection): %v", err)
	}
}

// X02 S2 tail (corpus rev 4): the server-list artifact's both eras run
// through dash's real decode. The valid fixture is the MI-2 envelope
// (dash's MaxSupportedMachineInterface era); the legacy fixture is the
// bare map-of-servers the pre-reshape CLI emitted — decode must not break
// against either, and the envelope's per-server fields (name + the S4
// stable id) must arrive intact.
func TestContractsCorpusServerListEnvelopeDecodes(t *testing.T) {
	raw := readCorpusFixture(t, "server-list-envelope/valid/mi2.json")
	records, err := cli.DecodeServerList(raw)
	if err != nil {
		t.Fatalf("server-list envelope fixture must decode through dash's server-list path: %v", err)
	}
	if len(records) != 2 || records[0].Name != "prod" || records[0].ID != "srv-0123456789abcdef" {
		t.Fatalf("server-list fixture records = %#v", records)
	}
	if records[1].Name != "staging" || records[1].ID != "" {
		t.Fatalf("id-less legacy entry must decode with an empty id: %#v", records[1])
	}
}

func TestContractsCorpusServerListLegacyBareMapDecodes(t *testing.T) {
	raw := readCorpusFixture(t, "server-list-envelope/legacy/bare-map.json")
	records, err := cli.DecodeServerList(raw)
	if err != nil {
		t.Fatalf("server-list legacy fixture must decode (pre-reshape CLI on PATH): %v", err)
	}
	if len(records) != 2 || records[0].Name != "prod" || records[0].ID != "srv-0123456789abcdef" {
		t.Fatalf("server-list legacy fixture records = %#v", records)
	}
}

// canonicalPreviewIDPattern is the CLI's canonical-era identity grammar
// (C06): <app>-p-<8 hex>. An ambiguous legacy record must NOT satisfy it --
// if it did, a consumer could silently "adopt" it as canonical, which is
// exactly the refusal the ambiguity class pins.
var canonicalPreviewIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*-p-[a-f0-9]{8}$`)

func TestContractsCorpusPreviewStateAmbiguousRefusesAdoption(t *testing.T) {
	raw := readCorpusFixture(t, "preview-state/ambiguous/two-branches-one-slug.json")
	data, err := cli.ParseJSON(raw)
	if err != nil {
		t.Fatalf("ambiguous preview fixture must decode as JSON: %v", err)
	}
	m, ok := data.(map[string]interface{})
	if !ok {
		t.Fatalf("ambiguous preview fixture decoded to %T, want object", data)
	}
	// The record stays what the producer wrote: legacy era, ambiguous, with
	// the candidate branches intact. Dash must never rewrite it into a
	// canonical identity (never adopt; explicit re-binding is the remedy).
	if era, _ := m["era"].(string); era != "legacy" {
		t.Fatalf("ambiguous fixture era = %q, want legacy (adoption would have rewritten it)", era)
	}
	id, _ := m["id"].(string)
	if canonicalPreviewIDPattern.MatchString(id) {
		t.Fatalf("ambiguous fixture id %q satisfies the canonical grammar; it must not be adoptable as canonical", id)
	}
	candidates, ok := m["candidates"].([]interface{})
	if !ok || len(candidates) != 2 {
		t.Fatalf("ambiguous fixture candidates = %#v, want the two competing branches", m["candidates"])
	}
	seen := map[string]bool{}
	for _, c := range candidates {
		branch, _ := c.(map[string]interface{})["branch"].(string)
		canon, _ := c.(map[string]interface{})["canonical_id"].(string)
		if branch == "" || canon == "" || !canonicalPreviewIDPattern.MatchString(canon) {
			t.Fatalf("ambiguous fixture candidate %#v is malformed", c)
		}
		if seen[branch] {
			t.Fatalf("duplicate candidate branch %q", branch)
		}
		seen[branch] = true
	}
	// The ambiguity is real: two branches, one slug -- exactly why adoption
	// must refuse and require explicit binding.
}
