package server

// sources.go — registered git sources and the inbound webhook delivery
// path. The source store owns canonical identity (forge kind + normalized
// clone URL — the F03 lesson), the credential REFERENCE and the webhook
// secret. Authenticated pushes are verified against the live branch head and
// recorded in a durable per-source ledger (dedupe by delivery id). A
// verified push for a repository an app builds from wakes that app's
// admitter on its server (`trigger poll --now`, the D4 relay); the poll —
// not dash — decides what deploys.

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
	if r.Method != http.MethodGet {
		s.sourceActionMu.Lock()
		defer s.sourceActionMu.Unlock()
	}
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
// (or GitLab token) IS the authentication. The durable ledger record is the
// commit point (200 only after fsync); an unavailable push-head check is not
// recorded so the forge's retry re-runs it; duplicate delivery ids answer
// "duplicate".
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
	if !deliveryAuthenticatedFor(src.Forge, r, secret, body) {
		// Unauthenticated deliveries are never recorded: an attacker could
		// spray fabricated delivery ids into the ledger otherwise. The
		// refusal is logged, nothing more.
		log.Printf("[source] rejecting unauthenticated delivery for %s", id)
		writeErrorStatus(w, "delivery signature invalid", http.StatusUnauthorized)
		return
	}

	s.sourceActionMu.Lock()
	defer s.sourceActionMu.Unlock()
	// Re-read after admission/deletion serialization. Authentication against
	// a rotated or deleted incarnation must never admit stale work.
	src, err = s.sources.Get(id)
	if err != nil {
		writeSourceError(w, err)
		return
	}
	currentSecret, err := s.sources.WebhookSecret(id)
	if err != nil || !deliveryAuthenticatedFor(src.Forge, r, currentSecret, body) {
		writeErrorStatus(w, "delivery authority changed", http.StatusUnauthorized)
		return
	}
	deliveryID := deliveryIDOf(r, body)
	event := source.NormalizeEvent(deliveryEventHeader(r))
	branch, commit := source.ParsePush(body)
	receivedAt := time.Now().UTC()

	if s.sources.DeliverySeen(id, deliveryID) {
		writeDeliveryStatus(w, http.StatusOK, "duplicate", "")
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
		writeDeliveryStatus(w, http.StatusOK, "ignored", reason)
	}

	if event == "pull_request" {
		ignore("pull request previews are not supported yet")
		return
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
		// Recorded, never silently dropped: the degraded state is visible
		// on the source.
		if err := s.sources.RecordDelivery(id, source.Delivery{
			ID: deliveryID, Event: event, Branch: branch, Commit: commit,
			Disposition: source.DispositionDegraded,
			Reason:      "source degraded: " + src.DegradeReason, ReceivedAt: receivedAt,
		}); err != nil {
			writeError(w, err.Error())
			return
		}
		writeDeliveryStatus(w, http.StatusOK, "ignored", "source degraded: "+src.DegradeReason)
		return
	}
	if src.DefaultBranch != "" && branch != "" && branch != src.DefaultBranch {
		// The default branch is an as-of-added snapshot: a forge-side
		// default-branch change does not silently retarget the source.
		ignore(fmt.Sprintf("push to %q does not match watched default branch %q", branch, src.DefaultBranch))
		return
	}
	if s.sourcePushAuthority != nil {
		err = s.sourcePushAuthority(r.Context(), src, branch, commit)
	} else {
		access, resolveErr := s.resolveSourceAccess(r.Context(), src)
		if resolveErr != nil {
			writeErrorStatus(w, "push authority unavailable", http.StatusServiceUnavailable)
			return
		}
		err = access.PushHead(r.Context(), src, branch, commit)
		access.Close()
	}
	if err != nil {
		if source.IsStaleAuthority(err) {
			ignore("push ref is no longer authoritative")
		} else {
			// Not recorded: the forge's retry with the same delivery id
			// re-runs verification instead of answering duplicate.
			writeErrorStatus(w, "push authority unavailable", http.StatusServiceUnavailable)
		}
		return
	}
	// D4: a verified push wakes the connected app's admitter (the relay
	// re-verifies the routing hint against the app's config, answers the
	// forge immediately, and records the poll result in the ledger).
	s.relayVerifiedPush(w, src, source.Delivery{
		ID: deliveryID, Event: event, Branch: branch, Commit: commit, ReceivedAt: receivedAt,
	})
}

func (s *Server) resolveSourceAccess(ctx context.Context, src *source.Source) (*source.Access, error) {
	if s.sourceAccessResolver != nil {
		return s.sourceAccessResolver(ctx, src)
	}
	return s.sourceProviders.Resolve(ctx, src)
}

// deliveryAuthenticated verifies the forge's own signature schemes against
// the raw body: X-Hub-Signature-256 (GitHub/Forgejo/Gitea HMAC-SHA256) or
// native Gitea/Forgejo signatures, and GitLab token or SHA256 signature.
func deliveryAuthenticatedFor(forge source.Forge, r *http.Request, secret string, body []byte) bool {
	if forge == source.ForgeGitLab {
		token := r.Header.Get("X-Gitlab-Token")
		if token != "" {
			return hmac.Equal([]byte(token), []byte(secret))
		}
		signature := strings.TrimPrefix(strings.TrimSpace(r.Header.Get("X-Gitlab-Signature")), "sha256=")
		expected, err := hex.DecodeString(signature)
		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write(body)
		return err == nil && len(expected) == sha256.Size && hmac.Equal(expected, mac.Sum(nil))
	}
	if forge == source.ForgeGitea || forge == source.ForgeForgejo {
		headers := []string{"X-Gitea-Signature"}
		if forge == source.ForgeForgejo {
			headers = append([]string{"X-Forgejo-Signature"}, headers...)
		}
		for _, header := range headers {
			if signature := strings.TrimSpace(r.Header.Get(header)); signature != "" {
				expected, err := hex.DecodeString(signature)
				mac := hmac.New(sha256.New, []byte(secret))
				mac.Write(body)
				return err == nil && len(expected) == sha256.Size && hmac.Equal(expected, mac.Sum(nil))
			}
		}
	}
	if r.Header.Get("X-Hub-Signature-256") == "" {
		return false
	}
	return deliveryAuthenticated(r, secret, body)
}

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
	for _, header := range []string{"X-Forgejo-Event", "X-Gitea-Event", "X-Gitlab-Event"} {
		if event := r.Header.Get(header); event != "" {
			return event
		}
	}
	return ""
}

// deliveryIDOf reads the provider's delivery identity. Without a delivery
// header, one is derived from the body digest so provider retries still
// dedupe.
func deliveryIDOf(r *http.Request, body []byte) string {
	if id := strings.TrimSpace(r.Header.Get("X-GitHub-Delivery")); id != "" {
		return id
	}
	for _, header := range []string{"X-Forgejo-Delivery", "X-Gitea-Delivery", "X-Gitlab-Event-UUID"} {
		if id := strings.TrimSpace(r.Header.Get(header)); id != "" {
			return id
		}
	}
	digest := sha256.Sum256(body)
	return "body:" + hex.EncodeToString(digest[:])[:16]
}

func writeDeliveryStatus(w http.ResponseWriter, status int, disposition, reason string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	payload := map[string]interface{}{"status": disposition, "disposition": disposition}
	if reason != "" {
		payload["reason"] = reason
	}
	json.NewEncoder(w).Encode(payload)
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

func ensureJSONEOF(decoder *json.Decoder) error {
	var extra interface{}
	if err := decoder.Decode(&extra); err != io.EOF {
		return fmt.Errorf("request body must contain one JSON object")
	}
	return nil
}
