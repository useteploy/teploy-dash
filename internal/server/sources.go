package server

// sources.go — D04 first slice: the git-source identity model and the
// inbound webhook delivery path. The source store owns canonical identity
// (forge kind + normalized clone URL — the F03 lesson), the credential
// REFERENCE and the webhook secret reference. Authenticated deliveries are
// recorded in a durable per-source ledger (dedupe by delivery id) and
// forwarded onto the EXISTING operation queue as manifest_apply operations
// carrying (source identity, authenticated commit) — reusing the D02
// admission machinery (idempotency, budgets, FIFO, reconciliation states),
// never a second queue. teploy-cli's server-side autodeploy (C02 admission
// ledger + commit pinning) stays the engine for its own webhook listener;
// dash layers identity + delivery honesty on top for dash-admitted work.

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/useteploy/teploy-dash/internal/manifest"
	"github.com/useteploy/teploy-dash/internal/operation"
	"github.com/useteploy/teploy-dash/internal/source"
)

type sourceCreateRequest struct {
	Forge         string `json:"forge"`
	CloneURL      string `json:"clone_url"`
	DefaultBranch string `json:"default_branch,omitempty"`
	CredentialRef string `json:"credential_ref,omitempty"`
	DisplayName   string `json:"display_name,omitempty"`
}

type sourceUpdateRequest struct {
	DefaultBranch *string `json:"default_branch,omitempty"`
	CredentialRef *string `json:"credential_ref,omitempty"`
	DisplayName   *string `json:"display_name,omitempty"`
}

func (s *Server) sourcesAvailable(w http.ResponseWriter) bool {
	if s.sources != nil {
		return true
	}
	message := "source service unavailable"
	if s.sourceInitErr != nil {
		message += ": " + s.sourceInitErr.Error()
	}
	writeErrorStatus(w, message, http.StatusServiceUnavailable)
	return false
}

// sourceView is the API envelope: the stored record plus the delivery
// identity surface (webhook URL, secret-present flag) — never the secret
// or any token value. WebhookSecret is populated EXACTLY ONCE, on the
// create and rotate responses.
type sourceView struct {
	source.Source
	WebhookURL        string            `json:"webhook_url"`
	WebhookSecretSet  bool              `json:"webhook_secret_set"`
	CredentialVerdict string            `json:"credential_verdict,omitempty"`
	WebhookSecret     string            `json:"webhook_secret,omitempty"`
	Deliveries        []source.Delivery `json:"deliveries,omitempty"`
}

func (s *Server) sourceViewOf(src source.Source, withDeliveries bool) sourceView {
	view := sourceView{
		Source:           src,
		WebhookURL:       "/hooks/sources/" + src.ID,
		WebhookSecretSet: true,
	}
	switch {
	case src.Degraded:
		view.CredentialVerdict = "degraded"
	case !src.DegradeCheckedAt.IsZero():
		view.CredentialVerdict = "verified"
	case src.CredentialRef != "":
		view.CredentialVerdict = "unverified"
	default:
		view.CredentialVerdict = "no-credential"
	}
	if withDeliveries && s.sources != nil {
		view.Deliveries = s.sources.RecentDeliveries(src.ID, 20)
	}
	return view
}

func (s *Server) handleSources(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	if !s.sourcesAvailable(w) {
		return
	}
	switch r.Method {
	case http.MethodGet:
		sources, err := s.sources.List()
		if err != nil {
			writeError(w, err.Error())
			return
		}
		views := make([]sourceView, 0, len(sources))
		for _, src := range sources {
			views = append(views, s.sourceViewOf(src, false))
		}
		writeData(w, views)
	case http.MethodPost:
		var request sourceCreateRequest
		decoder := json.NewDecoder(r.Body)
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&request); err != nil {
			writeErrorStatus(w, "invalid request body: "+err.Error(), http.StatusBadRequest)
			return
		}
		if err := ensureJSONEOF(decoder); err != nil {
			writeErrorStatus(w, err.Error(), http.StatusBadRequest)
			return
		}
		created, err := s.sources.Create(source.CreateInput{
			Forge:         source.Forge(request.Forge),
			CloneURL:      request.CloneURL,
			DefaultBranch: request.DefaultBranch,
			CredentialRef: request.CredentialRef,
			DisplayName:   request.DisplayName,
		})
		if err != nil {
			writeSourceError(w, err)
			return
		}
		view := s.sourceViewOf(created.Source, false)
		view.WebhookSecret = created.WebhookSecret
		w.Header().Set("Location", "/api/sources/"+created.ID)
		writeStatusBody(w, http.StatusCreated, view)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleSource(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	if !s.sourcesAvailable(w) {
		return
	}
	path := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/sources/"), "/")
	parts := strings.Split(path, "/")
	if len(parts) == 0 || !source.ValidID(parts[0]) {
		writeErrorStatus(w, "invalid source id", http.StatusBadRequest)
		return
	}
	id := parts[0]
	if len(parts) == 1 {
		switch r.Method {
		case http.MethodGet:
			src, err := s.sources.Get(id)
			if err != nil {
				writeSourceError(w, err)
				return
			}
			writeData(w, s.sourceViewOf(*src, true))
		case http.MethodPatch:
			var request sourceUpdateRequest
			decoder := json.NewDecoder(r.Body)
			decoder.DisallowUnknownFields()
			if err := decoder.Decode(&request); err != nil {
				writeErrorStatus(w, "invalid request body: "+err.Error(), http.StatusBadRequest)
				return
			}
			if err := ensureJSONEOF(decoder); err != nil {
				writeErrorStatus(w, err.Error(), http.StatusBadRequest)
				return
			}
			updated, err := s.sources.Update(id, source.UpdateInput{
				DefaultBranch: request.DefaultBranch,
				CredentialRef: request.CredentialRef,
				DisplayName:   request.DisplayName,
			})
			if err != nil {
				writeSourceError(w, err)
				return
			}
			writeData(w, s.sourceViewOf(*updated, false))
		case http.MethodDelete:
			if err := s.sources.Delete(id); err != nil {
				writeSourceError(w, err)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
		return
	}
	if len(parts) == 2 {
		switch parts[1] {
		case "verify":
			s.handleSourceVerify(w, r, id)
			return
		case "rotate-secret":
			s.handleSourceRotateSecret(w, r, id)
			return
		}
	}
	writeErrorStatus(w, "source route not found", http.StatusNotFound)
}

// handleSourceVerify runs the injected credential verifier and records the
// verdict on the source (D04 revoked-permission handling): a failure marks
// the source degraded with the exact reason — visible on every read, never
// a silent drop; a success clears it.
func (s *Server) handleSourceVerify(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !s.sourcesAvailable(w) {
		return
	}
	src, err := s.sources.Get(id)
	if err != nil {
		writeSourceError(w, err)
		return
	}
	if src.CredentialRef == "" {
		writeErrorStatus(w, "source has no credential reference to verify", http.StatusBadRequest)
		return
	}
	if s.sourceVerifier == nil {
		writeErrorStatus(w, "no credential verifier configured on this dashboard", http.StatusNotImplemented)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	if err := s.sourceVerifier(ctx, src); err != nil {
		if markErr := s.sources.MarkCredentialState(id, true, err.Error()); markErr != nil {
			writeError(w, markErr.Error())
			return
		}
		updated, _ := s.sources.Get(id)
		writeData(w, map[string]interface{}{"degraded": true, "reason": err.Error(), "source": s.sourceViewOf(*updated, false)})
		return
	}
	if err := s.sources.MarkCredentialState(id, false, ""); err != nil {
		writeError(w, err.Error())
		return
	}
	updated, _ := s.sources.Get(id)
	writeData(w, map[string]interface{}{"degraded": false, "source": s.sourceViewOf(*updated, false)})
}

// handleSourceRotateSecret replaces the webhook secret and returns the new
// value exactly once.
func (s *Server) handleSourceRotateSecret(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !s.sourcesAvailable(w) {
		return
	}
	secret, err := s.sources.RotateWebhookSecret(id)
	if err != nil {
		writeSourceError(w, err)
		return
	}
	writeData(w, map[string]string{"webhook_secret": secret})
}

// handleSourceWebhook is the inbound forge delivery endpoint — the ONLY
// unauthenticated route class besides health/status/MCP, because the HMAC
// (or GitLab token) IS the authentication. Delivery handling follows the
// C02 discipline: the durable ledger record is the commit point (200 only
// after fsync), refused admissions are never marked seen so the forge's
// retry re-runs admission, and duplicate delivery ids answer "duplicate".
func (s *Server) handleSourceWebhook(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !s.sourcesAvailable(w) {
		return
	}
	id := strings.Trim(strings.TrimPrefix(r.URL.Path, "/hooks/sources/"), "/")
	if !source.ValidID(id) {
		writeErrorStatus(w, "unknown source", http.StatusNotFound)
		return
	}
	src, err := s.sources.Get(id)
	if err != nil {
		writeSourceError(w, err)
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeErrorStatus(w, "unreadable delivery", http.StatusBadRequest)
		return
	}
	secret, err := s.sources.WebhookSecret(id)
	if err != nil {
		writeErrorStatus(w, "source webhook secret unavailable", http.StatusInternalServerError)
		return
	}
	if !deliveryAuthenticated(r, secret, body) {
		// Unauthenticated deliveries are never recorded: an attacker could
		// spray fabricated delivery ids into the ledger otherwise. The
		// refusal is logged, nothing more.
		log.Printf("[source] rejecting unauthenticated delivery for %s", id)
		writeErrorStatus(w, "delivery signature invalid", http.StatusUnauthorized)
		return
	}

	deliveryID := deliveryIDOf(r, body)
	event := source.NormalizeEvent(deliveryEventHeader(r))
	branch, commit := source.ParsePush(body)
	receivedAt := time.Now().UTC()

	if s.sources.DeliverySeen(id, deliveryID) {
		writeDeliveryStatus(w, http.StatusOK, "duplicate", "", nil)
		return
	}

	ignore := func(reason string) {
		if err := s.sources.RecordDelivery(id, source.Delivery{
			ID: deliveryID, Event: event, Branch: branch, Commit: commit,
			Disposition: source.DispositionIgnored, Reason: reason, ReceivedAt: receivedAt,
		}); err != nil {
			writeError(w, err.Error())
			return
		}
		writeDeliveryStatus(w, http.StatusOK, "ignored", reason, nil)
	}

	if event != "push" {
		ignore(fmt.Sprintf("event %q is not a push", event))
		return
	}
	if commit == "" {
		ignore(fmt.Sprintf("push to %q carries no pinnable commit (tag, deletion, or malformed hash)", branch))
		return
	}
	if src.Degraded {
		// Recorded and refused admission — the degraded state is visible on
		// the source; the delivery is never silently dropped.
		if err := s.sources.RecordDelivery(id, source.Delivery{
			ID: deliveryID, Event: event, Branch: branch, Commit: commit,
			Disposition: source.DispositionDegraded,
			Reason:      "source degraded: " + src.DegradeReason, ReceivedAt: receivedAt,
		}); err != nil {
			writeError(w, err.Error())
			return
		}
		writeDeliveryStatus(w, http.StatusOK, "ignored", "source degraded: "+src.DegradeReason, nil)
		return
	}
	if src.DefaultBranch != "" && branch != "" && branch != src.DefaultBranch {
		// The default branch is an as-of-added snapshot: a forge-side
		// default-branch change does not silently retarget deploys — the
		// operator updates the source deliberately.
		ignore(fmt.Sprintf("push to %q does not match watched default branch %q", branch, src.DefaultBranch))
		return
	}

	bound, err := s.manifestsBoundTo(src)
	if err != nil {
		writeErrorStatus(w, "manifest service unavailable: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	if len(bound) == 0 {
		ignore("no git-managed manifest is bound to this source")
		return
	}

	// Forward onto the EXISTING operation queue (D02 machinery): one
	// manifest_apply per bound manifest, each carrying the source identity
	// and the authenticated commit. The delivery-scoped idempotency key
	// makes replays converge even if the ledger record was lost.
	var operationIDs []string
	var refused error
	for _, metadata := range bound {
		key := webhookIdempotencyKey(deliveryID, metadata.Server, metadata.App)
		// Newest-delivery supersede (C02's documented newest-wins): cancel
		// still-QUEUED operations this source admitted for the same target
		// — the running one is never interrupted; the newest runs next.
		// Operations from THIS delivery (a ledger-failure retry) are
		// identified by their idempotency key and left alone.
		s.supersedeQueuedSourceOps(src.ID, "server:"+metadata.Server+"/app:"+metadata.App, key, deliveryID)
		op, _, err := s.operations.Enqueue(operation.Request{
			Kind:             operation.KindManifestApply,
			Server:           metadata.Server,
			App:              metadata.App,
			Mode:             string(manifest.ModeGitManaged),
			ManifestRevision: metadata.CurrentRevision,
			SourceID:         src.ID,
			SourceCommit:     commit,
		}, key, &operation.Actor{Kind: "webhook", Subject: "source/" + src.ID, Label: src.DisplayName})
		if err != nil {
			refused = err
			break
		}
		operationIDs = append(operationIDs, op.ID)
	}
	if refused != nil {
		// Nothing (or only part of the batch) was admitted for this
		// delivery: record the refusal WITHOUT marking the delivery seen so
		// the forge's retry re-runs admission (C02's rollback rule).
		if err := s.sources.RecordDelivery(id, source.Delivery{
			ID: deliveryID, Event: event, Branch: branch, Commit: commit,
			Disposition: source.DispositionRefused, Reason: refused.Error(), ReceivedAt: receivedAt,
		}); err != nil {
			writeError(w, err.Error())
			return
		}
		writeRefusedDelivery(w, refused)
		return
	}
	if err := s.sources.RecordDelivery(id, source.Delivery{
		ID: deliveryID, Event: event, Branch: branch, Commit: commit,
		Disposition: source.DispositionAdmitted, ReceivedAt: receivedAt,
		OperationID: strings.Join(operationIDs, ","),
	}); err != nil {
		writeError(w, err.Error())
		return
	}
	writeDeliveryStatus(w, http.StatusOK, "admitted", "", operationIDs)
}

// supersedeQueuedSourceOps cancels the still-QUEUED operations one source
// admitted for one target, excluding the incoming delivery's own key (a
// retry after a ledger failure replays, it does not supersede itself).
// Canceled operations keep their honest D02 status; the delivery ledger
// carries the supersede marker.
func (s *Server) supersedeQueuedSourceOps(sourceID, target, incomingKey, newDeliveryID string) {
	if s.operations == nil {
		return
	}
	for _, op := range s.operations.List(operation.StatusQueued, target, 0) {
		if op.Request.SourceID != sourceID || op.IdempotencyKey == incomingKey {
			continue
		}
		if _, err := s.operations.Cancel(op.ID); err != nil {
			log.Printf("[source] supersede cancel of %s failed: %v", op.ID, err)
			continue
		}
		log.Printf("[source] delivery %s superseded queued operation %s", newDeliveryID, op.ID)
		if err := s.sources.RecordDelivery(sourceID, source.Delivery{
			ID:          deliveryIDFromKey(op.IdempotencyKey),
			Event:       "push",
			Disposition: source.DispositionSuperseded,
			Reason:      "superseded by delivery " + newDeliveryID,
			OperationID: op.ID,
			ReceivedAt:  time.Now().UTC(),
		}); err != nil {
			log.Printf("[source] recording supersede marker failed: %v", err)
		}
	}
}

func deliveryIDFromKey(key string) string {
	if !strings.HasPrefix(key, "wh:") {
		return key
	}
	rest := strings.TrimPrefix(key, "wh:")
	if idx := strings.LastIndex(rest, ":"); idx > 0 {
		return rest[:idx]
	}
	return rest
}

func webhookIdempotencyKey(deliveryID, server, app string) string {
	return "wh:" + deliveryID + ":" + server + "/" + app
}

// manifestsBoundTo joins a source against the git-managed manifests whose
// git.repository canonical URL matches (transport- and case-folded). A
// manifest whose repository URL cannot be canonicalized fails loudly: the
// whole delivery errors rather than half-matching.
func (s *Server) manifestsBoundTo(src *source.Source) ([]manifest.Metadata, error) {
	if s.manifests == nil {
		return nil, fmt.Errorf("manifest service unavailable")
	}
	want, err := source.CanonicalURL(src.CloneURL)
	if err != nil {
		return nil, fmt.Errorf("source clone URL no longer canonicalizes: %w", err)
	}
	metadatas, err := s.manifests.List()
	if err != nil {
		return nil, err
	}
	var bound []manifest.Metadata
	for _, metadata := range metadatas {
		if metadata.Mode != manifest.ModeGitManaged || metadata.Git == nil {
			continue
		}
		got, err := source.CanonicalURL(metadata.Git.Repository)
		if err != nil {
			return nil, fmt.Errorf("manifest %s/%s has an uncanonicalizable git repository: %w", metadata.Server, metadata.App, err)
		}
		if got == want {
			bound = append(bound, metadata)
		}
	}
	return bound, nil
}

// deliveryAuthenticated verifies the forge's own signature schemes against
// the raw body: X-Hub-Signature-256 (GitHub/Forgejo/Gitea HMAC-SHA256) or
// X-Gitlab-Token (shared secret, constant-time).
func deliveryAuthenticated(r *http.Request, secret string, body []byte) bool {
	if signature := strings.TrimSpace(r.Header.Get("X-Hub-Signature-256")); signature != "" {
		if !strings.HasPrefix(signature, "sha256=") {
			return false
		}
		expected, err := hex.DecodeString(strings.TrimPrefix(signature, "sha256="))
		if err != nil {
			return false
		}
		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write(body)
		return hmac.Equal(mac.Sum(nil), expected)
	}
	if token := r.Header.Get("X-Gitlab-Token"); token != "" {
		return hmac.Equal([]byte(token), []byte(secret))
	}
	return false
}

// deliveryEventHeader reads the provider's event name (header spelling
// differs per forge; normalization happens in the source package).
func deliveryEventHeader(r *http.Request) string {
	if event := r.Header.Get("X-GitHub-Event"); event != "" {
		return event
	}
	return r.Header.Get("X-Gitlab-Event")
}

// deliveryIDOf reads the provider's delivery identity. Without a delivery
// header, one is derived from the body digest so provider retries still
// dedupe.
func deliveryIDOf(r *http.Request, body []byte) string {
	if id := strings.TrimSpace(r.Header.Get("X-GitHub-Delivery")); id != "" {
		return id
	}
	if id := strings.TrimSpace(r.Header.Get("X-Gitlab-Event-UUID")); id != "" {
		return id
	}
	digest := sha256.Sum256(body)
	return "body:" + hex.EncodeToString(digest[:])[:16]
}

func writeDeliveryStatus(w http.ResponseWriter, status int, disposition, reason string, operationIDs []string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	payload := map[string]interface{}{"status": disposition}
	if reason != "" {
		payload["reason"] = reason
	}
	if len(operationIDs) > 0 {
		payload["operation_ids"] = operationIDs
	}
	json.NewEncoder(w).Encode(payload)
}

func writeRefusedDelivery(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, operation.ErrAdmissionBudget), errors.Is(err, operation.ErrGlobalAdmissionBudget):
		writeErrorStatus(w, err.Error(), http.StatusTooManyRequests)
	case errors.Is(err, operation.ErrShuttingDown):
		writeErrorStatus(w, err.Error(), http.StatusServiceUnavailable)
	default:
		writeError(w, err.Error())
	}
}

func writeStatusBody(w http.ResponseWriter, status int, body interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]interface{}{"data": body})
}

func writeSourceError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, source.ErrNotFound):
		writeErrorStatus(w, err.Error(), http.StatusNotFound)
	case errors.Is(err, source.ErrDuplicate):
		writeErrorStatus(w, err.Error(), http.StatusConflict)
	default:
		writeError(w, err.Error())
	}
}
