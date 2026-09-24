package server

// granted_at_test.go — D06 granted-at audit trail: every admitted operation
// records the capability set the principal was authorized under AT
// ADMISSION, plus how that set was derived (role + basis). For SSO
// principals the role arrived from the identity provider's claim, so Role +
// CapBasis ARE the recorded claim-to-capability mapping.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/useteploy/teploy-dash/internal/caps"
	"github.com/useteploy/teploy-dash/internal/mcp"
	"github.com/useteploy/teploy-dash/internal/operation"
)

func enqueueBody() string {
	return `{"kind":"deploy","server":"prod","app":"web","image":"example/web:1"}`
}

func decodeOperation(t *testing.T, rec *httptest.ResponseRecorder) *operation.Operation {
	t.Helper()
	if rec.Code != http.StatusAccepted {
		t.Fatalf("enqueue status = %d body=%s", rec.Code, rec.Body.String())
	}
	var envelope struct {
		Data operation.Operation `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	return &envelope.Data
}

// A preset local account: the granted-at set is the role's preset, basis
// "preset", and the snapshot matches the LIVE enforcement set for the same
// request.
func TestGrantedAtSnapshot_PresetLocalAccount(t *testing.T) {
	s := newCapsServer(t, t.TempDir())
	if err := s.gate.createUser("op", "oppass1234", RoleEditor); err != nil {
		t.Fatal(err)
	}
	cookie := loginCookie(t, s.gate, "op", "oppass1234")

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/operations", strings.NewReader(enqueueBody()))
	req.AddCookie(cookie)
	s.handler().ServeHTTP(rec, req)
	op := decodeOperation(t, rec)

	if op.Actor == nil {
		t.Fatal("operation carries no actor")
	}
	if op.Actor.CapBasis != "preset" || op.Actor.Role != RoleEditor {
		t.Fatalf("actor basis/role = %q/%q, want preset/editor", op.Actor.CapBasis, op.Actor.Role)
	}
	want := caps.PresetForRole(RoleEditor).Sorted()
	if strings.Join(op.Actor.Capabilities, ",") != strings.Join(want, ",") {
		t.Fatalf("granted-at capabilities = %v, want the editor preset %v", op.Actor.Capabilities, want)
	}
	waitForTerminalOperation(t, s, op.ID)
}

// A legacy (pre-X03) account: the snapshot records the FROZEN legacy basis —
// the audit trail must show the account was operating under the legacy
// profile, not silently rewrite it as a preset. The dummy hash's password
// is unknown, so the session is issued directly (the wrap() rebuild fills
// caps + basis from the loaded legacy row, exactly as for a real session).
func TestGrantedAtSnapshot_LegacyLocalAccount(t *testing.T) {
	dir := t.TempDir()
	writeLegacyUsers(t, dir, map[string]string{"led": RoleEditor})
	s := newCapsServer(t, dir)

	s.gate.credMu.RLock()
	epoch := s.gate.users["led"].AuthEpoch
	s.gate.credMu.RUnlock()
	token := s.gate.newSessionFor("led", "led", RoleEditor, epoch, true)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/operations", strings.NewReader(enqueueBody()))
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: token})
	req.Host = "dash.local"
	s.handler().ServeHTTP(rec, req)
	op := decodeOperation(t, rec)

	if op.Actor.CapBasis != "legacy" || op.Actor.Role != RoleEditor {
		t.Fatalf("actor basis/role = %q/%q, want legacy/editor", op.Actor.CapBasis, op.Actor.Role)
	}
	want := caps.LegacyForRole(RoleEditor).Sorted()
	if strings.Join(op.Actor.Capabilities, ",") != strings.Join(want, ",") {
		t.Fatalf("granted-at capabilities = %v, want the legacy editor set %v", op.Actor.Capabilities, want)
	}
	waitForTerminalOperation(t, s, op.ID)
}

// An SSO principal: the role recorded on the actor is the claim-derived role
// — the claim-to-capability mapping evidence.
func TestGrantedAtSnapshot_SSOPrincipalRecordsClaimRole(t *testing.T) {
	s := newCapsServer(t, t.TempDir())
	seedPresetUsers(t, s) // a local account clears first-run setup mode
	sub := oidcSubjectID("https://idp.example", "u-grant")
	epoch := mintSSO(t, s, sub, "gina", RoleEditor)

	rec := ssoDo(t, s, sub, "gina", RoleEditor, http.MethodPost, "/api/operations", enqueueBody(), epoch)
	op := decodeOperation(t, rec)

	if op.Actor.Kind != "sso" || op.Actor.Subject != sub {
		t.Fatalf("actor = %+v, want sso/%s", op.Actor, sub)
	}
	if op.Actor.Role != RoleEditor || op.Actor.CapBasis != "preset" {
		t.Fatalf("claim-mapped role/basis = %q/%q, want editor/preset (the IdP role claim maps onto the preset)", op.Actor.Role, op.Actor.CapBasis)
	}
	want := caps.PresetForRole(RoleEditor).Sorted()
	if strings.Join(op.Actor.Capabilities, ",") != strings.Join(want, ",") {
		t.Fatalf("granted-at capabilities = %v, want %v", op.Actor.Capabilities, want)
	}
	waitForTerminalOperation(t, s, op.ID)
}

// An MCP token: nil capabilities are the operator-default preset; an
// explicit list is the custom grant. Both snapshot onto the actor.
func TestGrantedAtSnapshot_MCPToken(t *testing.T) {
	s := newCapsServer(t, t.TempDir())

	backend := mcpBackend{s: s}
	// Default-mint token (nil capabilities -> operator preset).
	ctx := mcp.WithToken(context.Background(), mcp.Token{ID: "tok-default", Name: "ci"})
	out, err := backend.enqueueMutation(ctx, operation.Request{Kind: operation.KindDeploy, Server: "prod", App: "web", Image: "example/web:1"})
	if err != nil {
		t.Fatal(err)
	}
	var defOp operation.Operation
	if err := json.Unmarshal([]byte(out), &defOp); err != nil {
		t.Fatal(err)
	}
	if defOp.Actor.CapBasis != "preset" {
		t.Fatalf("default token basis = %q, want preset", defOp.Actor.CapBasis)
	}
	if want := caps.OperatorDefault().Sorted(); strings.Join(defOp.Actor.Capabilities, ",") != strings.Join(want, ",") {
		t.Fatalf("default token granted-at = %v, want the operator preset %v", defOp.Actor.Capabilities, want)
	}
	waitForTerminalOperation(t, s, defOp.ID)

	// Scoped token (explicit capabilities -> custom).
	ctx2 := mcp.WithToken(context.Background(), mcp.Token{ID: "tok-scoped", Name: "mini", Capabilities: []string{caps.ViewMetadata}})
	out2, err := backend.enqueueMutation(ctx2, operation.Request{Kind: operation.KindDeploy, Server: "prod", App: "web", Image: "example/web:2"})
	if err != nil {
		t.Fatal(err)
	}
	var scopedOp operation.Operation
	if err := json.Unmarshal([]byte(out2), &scopedOp); err != nil {
		t.Fatal(err)
	}
	if scopedOp.Actor.CapBasis != "custom" || strings.Join(scopedOp.Actor.Capabilities, ",") != caps.ViewMetadata {
		t.Fatalf("scoped token granted-at = %v (%s), want [view.metadata] (custom)", scopedOp.Actor.Capabilities, scopedOp.Actor.CapBasis)
	}
	waitForTerminalOperation(t, s, scopedOp.ID)
}

// The snapshot is frozen at admission: narrowing the account after the
// operation was admitted does not rewrite the recorded grant.
func TestGrantedAtSnapshot_IsFrozenAtAdmission(t *testing.T) {
	s := newCapsServer(t, t.TempDir())
	if err := s.gate.createUser("nar", "narpass1234", RoleAdmin); err != nil {
		t.Fatal(err)
	}
	cookie := loginCookie(t, s.gate, "nar", "narpass1234")

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/operations", strings.NewReader(enqueueBody()))
	req.AddCookie(cookie)
	s.handler().ServeHTTP(rec, req)
	op := decodeOperation(t, rec)
	waitForTerminalOperation(t, s, op.ID)

	// Narrow the account onto a custom, minimal set.
	if err := s.gate.setCapabilities("nar", []string{caps.ViewMetadata}); err != nil {
		t.Fatalf("setCapabilities: %v", err)
	}

	stored, err := s.operations.Get(op.ID)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(stored.Actor.Capabilities, ",") != strings.Join(caps.PresetForRole(RoleAdmin).Sorted(), ",") || stored.Actor.CapBasis != "preset" {
		t.Fatalf("admitted grant was rewritten after narrowing: %+v", stored.Actor)
	}
}
