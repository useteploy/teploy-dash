package server

// gitsource.go — server-authority D4: the dash surface over the CLI's git
// sources. One admitter lives on the app's server (`teploy trigger poll`);
// these routes connect a repository (deploy key, host-key pin, forge
// registration — the git: block itself is set through the CAS config
// endpoint like any other settings edit) and surface the source's public
// state. Dash stores no app facts: the repo→app hints in the source
// routing cache are re-verified against the app's config before the
// webhook relay polls anything.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/useteploy/teploy-dash/internal/cli"
	"github.com/useteploy/teploy-dash/internal/configkeys"
	"github.com/useteploy/teploy-dash/internal/source"
)

var (
	gitRefPattern          = regexp.MustCompile(`^refs/heads/[^\s\x00]{1,122}$`)
	hostKeyFingerprintRule = regexp.MustCompile(`^SHA256:[A-Za-z0-9+/=]{10,100}$`)
	gitBranchRule          = regexp.MustCompile(`^[^\s\x00]{1,128}$`)
)

// gitTriggerCheckName is the doctor check reporting whether the server's
// git-source poll timer is installed (teploy v0.1.45+).
const gitTriggerCheckName = "git-trigger"

// gitSourceShowDTO mirrors the CLI's `config source show --json` envelope
// (contracts/schema/source-show-envelope.schema.json). Only the fields the
// relay/routing needs are decoded; the envelope passes through verbatim.
type gitSourceShowDTO struct {
	Repository string `json:"repository"`
	Branch     string `json:"branch"`
}

// requireGitSource is requireFeature for the git-source tokens (the v0.1.45
// commands); trigger routes additionally need the poll token.
func (s *Server) requireGitSource(w http.ResponseWriter, r *http.Request, what string, trigger bool) bool {
	if !s.cliInstalled() {
		writeError(w, "teploy CLI not installed")
		return false
	}
	features := s.capabilities(r.Context()).Features
	ok := features.GitSource && (!trigger || features.TriggerPoll)
	if !ok {
		writeErrorStatus(w, what+" needs a newer teploy CLI on the dashboard host (upgrade teploy to v0.1.45 or later)", http.StatusNotImplemented)
		return false
	}
	return true
}

// writeGitSourceCLIError maps a failed git-source CLI call: a completed
// non-zero exit is the CLI's operator-actionable refusal (422 with the
// machine detail or stderr text); anything else is a transport failure
// (502).
func writeGitSourceCLIError(w http.ResponseWriter, res *cli.Result, err error) {
	var exited *cli.ExitStatusError
	if !errors.As(err, &exited) {
		writeErrorStatus(w, err.Error(), http.StatusBadGateway)
		return
	}
	_, detail := cliMachineError(res)
	if detail == "" {
		detail = strings.TrimSpace(res.Stderr)
	}
	if detail == "" {
		detail = err.Error()
	}
	writeErrorStatus(w, detail, http.StatusUnprocessableEntity)
}

// handleAppSourceRoutes dispatches /api/apps/{server}/{app}/source...
// routes. Reports false when the action is not one of them.
func (s *Server) handleAppSourceRoutes(w http.ResponseWriter, r *http.Request, serverName, appName, action string) bool {
	switch {
	case action == "source" && r.Method == http.MethodGet:
		s.handleAppSourceShow(w, r, serverName, appName)
	case action == "source/timer" && r.Method == http.MethodGet:
		s.handleAppSourceTimer(w, r, serverName)
	case action == "source/key" && r.Method == http.MethodPost:
		s.handleAppSourceKey(w, r, serverName, appName)
	case action == "source/host-key" && r.Method == http.MethodPost:
		s.handleAppSourceHostKey(w, r, serverName, appName)
	case action == "source/register" && r.Method == http.MethodPost:
		s.handleAppSourceRegister(w, r, serverName, appName)
	case action == "source/reset-watermark" && r.Method == http.MethodPost:
		s.handleAppSourceReset(w, r, serverName, appName)
	case action == "source/retry" && r.Method == http.MethodPost:
		s.handleAppSourceRetry(w, r, serverName, appName)
	case action == "source/install-timer" && r.Method == http.MethodPost:
		s.handleAppSourceInstallTimer(w, r, serverName)
	default:
		return false
	}
	return true
}

// noteSourceRoute records (or clears) the repo→app routing hint from the
// app's own git: block as the CLI just read it.
func (s *Server) noteSourceRoute(serverName, appName, repository string) {
	if s.routing == nil {
		return
	}
	if repository == "" {
		if err := s.routing.Unlink(serverName, appName); err != nil {
			log.Printf("[source] routing unlink %s/%s: %v", serverName, appName, err)
		}
		return
	}
	if err := s.routing.Link(serverName, appName, repository); err != nil {
		log.Printf("[source] routing link %s/%s: %v", serverName, appName, err)
	}
}

// handleAppSourceShow serves GET .../source: the CLI's source-show
// envelope (connected repository, credential fingerprint, watermark)
// verbatim, with the routing hint refreshed from its repository field.
func (s *Server) handleAppSourceShow(w http.ResponseWriter, r *http.Request, serverName, appName string) {
	if !s.requireGitSource(w, r, "Repository status", false) {
		return
	}
	res, err := s.cliAppRun(r.Context(), serverName, appName, "config", "source", "show", "--json")
	if err != nil {
		writeGitSourceCLIError(w, res, err)
		return
	}
	var show gitSourceShowDTO
	if json.Unmarshal([]byte(strings.TrimSpace(res.Stdout)), &show) == nil {
		s.noteSourceRoute(serverName, appName, show.Repository)
	}
	writeRawJSON(w, res.Stdout)
}

// gitTimerEntry caches one server's doctor-derived poll-timer verdict.
type gitTimerEntry struct {
	installed bool
	known     bool
	detail    string
	builtAt   time.Time
}

// gitTriggerTimer reads (and caches per server, 10 minutes) whether the
// server's git-source poll timer is installed, from the doctor's
// git-trigger check. known is false when the report could not say.
func (s *Server) gitTriggerTimer(ctx context.Context, serverName string) gitTimerEntry {
	s.gitTimerMu.Lock()
	if s.gitTimer == nil {
		s.gitTimer = make(map[string]gitTimerEntry)
	}
	cached, ok := s.gitTimer[serverName]
	s.gitTimerMu.Unlock()
	if ok && time.Since(cached.builtAt) < 10*time.Minute {
		return cached
	}
	entry := gitTimerEntry{builtAt: time.Now().UTC()}
	srv, err := s.lookupServerStrict(ctx, serverName)
	if err == nil {
		args := []string{"doctor", "--host", srv.Host, "--json"}
		if srv.User != "" {
			args = append(args, "--user", srv.User)
		}
		// Doctor exits 1 when any check fails — the report is still the
		// answer, so only transport errors (nil result) lose the verdict.
		var res *cli.Result
		res, err = s.runCLI(ctx, args...)
		if err != nil && res == nil {
			err = fmt.Errorf("doctor did not complete: %w", err)
		} else if res != nil {
			err = nil
			var report struct {
				Checks []struct {
					Name   string `json:"name"`
					Result string `json:"result"`
					Detail string `json:"detail"`
				} `json:"checks"`
			}
			if json.Unmarshal([]byte(strings.TrimSpace(res.Stdout)), &report) != nil {
				entry.detail = "doctor report unreadable"
			}
			for _, check := range report.Checks {
				if check.Name != gitTriggerCheckName {
					continue
				}
				entry.known = true
				entry.installed = check.Result == "ok"
				entry.detail = check.Detail
				break
			}
		}
	}
	if err != nil {
		entry.detail = err.Error()
	}
	s.gitTimerMu.Lock()
	s.gitTimer[serverName] = entry
	s.gitTimerMu.Unlock()
	return entry
}

// handleAppSourceTimer serves GET .../source/timer: whether the server's
// poll timer is installed (the doctor's git-trigger check). When it is
// missing, the UI offers `trigger install-timer`.
func (s *Server) handleAppSourceTimer(w http.ResponseWriter, r *http.Request, serverName string) {
	if !s.requireGitSource(w, r, "Server poll timer", false) {
		return
	}
	entry := s.gitTriggerTimer(r.Context(), serverName)
	writeData(w, map[string]interface{}{
		"installed": entry.installed,
		"known":     entry.known,
		"detail":    entry.detail,
	})
}

// handleAppSourceKey serves POST .../source/key: `config source key`
// generates the app's ed25519 deploy key on the server (the private half
// never leaves it) and returns the public half. SSH deploy keys only; the
// HTTPS token credential has no dash surface yet.
func (s *Server) handleAppSourceKey(w http.ResponseWriter, r *http.Request, serverName, appName string) {
	if !s.requireGitSource(w, r, "Generating the deploy key", false) {
		return
	}
	var body struct {
		Type string `json:"type"`
	}
	if err := strictDecode(r, &body); err != nil {
		writeError(w, "invalid request body")
		return
	}
	if body.Type != "" && body.Type != "ssh" {
		writeError(w, "only ssh deploy keys are supported from the dashboard")
		return
	}
	args := append([]string{"config", "source", "key", "--type", "ssh", "--json"}, s.requestActorArgs(r)...)
	res, err := s.cliAppRun(r.Context(), serverName, appName, args...)
	if err != nil {
		writeGitSourceCLIError(w, res, err)
		return
	}
	writeRawJSON(w, res.Stdout)
}

// handleAppSourceHostKey serves POST .../source/host-key: `config source
// host-key` pins the forge's SSH host keys on the server. An
// expected_fingerprint (the operator verified it out of band) makes the
// pin refuse a host presenting anything else; without one the pin is
// trust-on-first-use and the UI shows the fingerprints for confirmation.
func (s *Server) handleAppSourceHostKey(w http.ResponseWriter, r *http.Request, serverName, appName string) {
	if !s.requireGitSource(w, r, "Pinning the forge host key", false) {
		return
	}
	var body struct {
		ExpectedFingerprint string `json:"expected_fingerprint"`
	}
	if err := strictDecode(r, &body); err != nil {
		writeError(w, "invalid request body")
		return
	}
	if body.ExpectedFingerprint != "" && !hostKeyFingerprintRule.MatchString(body.ExpectedFingerprint) {
		writeError(w, "expected_fingerprint must look like SHA256:...")
		return
	}
	args := []string{"config", "source", "host-key", "--json"}
	if body.ExpectedFingerprint != "" {
		args = append(args, "--expected-fingerprint", body.ExpectedFingerprint)
	}
	args = append(args, s.requestActorArgs(r)...)
	res, err := s.cliAppRun(r.Context(), serverName, appName, args...)
	if err != nil {
		writeGitSourceCLIError(w, res, err)
		return
	}
	writeRawJSON(w, res.Stdout)
}

// handleAppSourceRegister serves POST .../source/register: register the
// deploy key and the push webhook through the forge API when dash holds a
// credential for that repository; otherwise create the source anyway and
// return the webhook URL (and, for credential-admins, the once-only
// webhook secret) for the operator to paste.
func (s *Server) handleAppSourceRegister(w http.ResponseWriter, r *http.Request, serverName, appName string) {
	if !s.requireGitSource(w, r, "Registering the repository", false) {
		return
	}
	var body struct {
		Forge       string `json:"forge"`
		Repository  string `json:"repository"`
		Branch      string `json:"branch"`
		WebhookBase string `json:"webhook_base"`
		PublicKey   string `json:"public_key"`
	}
	if err := strictDecode(r, &body); err != nil {
		writeError(w, "invalid request body")
		return
	}
	forge := source.Forge(body.Forge)
	if !source.ValidForge(forge) || forge == source.ForgeGeneric {
		writeError(w, "forge must be github, gitlab, gitea or forgejo")
		return
	}
	if body.PublicKey == "" || len(body.PublicKey) > 8192 || strings.ContainsAny(body.PublicKey, "\r\n\x00") {
		writeError(w, "public_key is required")
		return
	}
	if !gitBranchRule.MatchString(body.Branch) {
		writeError(w, "branch is required")
		return
	}
	normalized, err := source.NormalizeCloneURL(body.Repository)
	if err != nil {
		writeError(w, err.Error())
		return
	}
	base, err := url.Parse(body.WebhookBase)
	if err != nil || (base.Scheme != "https" && base.Scheme != "http") || base.Host == "" ||
		base.User != nil || base.RawQuery != "" || base.Fragment != "" || base.Path != "" && base.Path != "/" {
		writeError(w, "webhook_base must be the dashboard's http(s) origin (scheme://host[:port])")
		return
	}
	if !s.sourcesAvailable(w) {
		return
	}

	// Watch only this branch while it is the sole app on the repository;
	// a second app on another branch widens the watch and the relay filters.
	watch := body.Branch
	for _, route := range s.routingRoutesFor(normalized) {
		if route.Server != serverName || route.App != appName {
			watch = ""
			break
		}
	}
	src, credentialRef, hadCredential, err := s.gitSourceEnsure(forge, normalized, watch)
	if err != nil {
		writeSourceError(w, err)
		return
	}
	s.noteSourceRoute(serverName, appName, normalized)
	webhookURL := strings.TrimRight(body.WebhookBase, "/") + "/hooks/sources/" + src.ID

	if !hadCredential {
		// Nothing dash holds can talk to this forge's API: hand the
		// operator the pieces to paste. The webhook secret is shown to
		// credential-admins only (the Sources page gate).
		secret := ""
		if canWritePrivilegedConfig(r) {
			secret, err = s.sources.WebhookSecret(src.ID)
			if err != nil {
				writeError(w, err.Error())
				return
			}
		}
		writeData(w, map[string]interface{}{
			"registered": false, "source_id": src.ID, "webhook_url": webhookURL,
			"public_key": body.PublicKey, "webhook_secret": secret,
		})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	access, err := s.resolveSourceAccess(ctx, src)
	if err != nil {
		writeErrorStatus(w, "forge credential unavailable: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer access.Close()
	title := fmt.Sprintf("teploy %s (%s)", appName, serverName)
	if err := access.RegisterDeployKey(ctx, src, title, body.PublicKey); err != nil {
		writeErrorStatus(w, "registering the deploy key: "+err.Error(), http.StatusBadGateway)
		return
	}
	secret, err := s.sources.WebhookSecret(src.ID)
	if err != nil {
		writeError(w, err.Error())
		return
	}
	branchFilter := body.Branch
	if watch == "" {
		branchFilter = ""
	}
	if err := access.RegisterWebhook(ctx, src, webhookURL, secret, branchFilter); err != nil {
		writeErrorStatus(w, "registering the webhook: "+err.Error(), http.StatusBadGateway)
		return
	}
	writeData(w, map[string]interface{}{
		"registered": true, "source_id": src.ID, "credential_ref": credentialRef, "webhook_url": webhookURL,
	})
}

// gitSourceEnsure returns the source for (forge, canonical URL), creating
// it when absent. hadCredential reports whether dash holds a provider
// credential for the repository; a created source carries its reference so
// verification and the relay work from the start.
func (s *Server) gitSourceEnsure(forge source.Forge, normalized, watch string) (*source.Source, string, bool, error) {
	ref, _, ok := s.sourceProviders.CredentialRefFor(normalized)
	id, err := source.DeriveID(forge, normalized)
	if err != nil {
		return nil, "", false, err
	}
	if src, err := s.sources.Get(id); err == nil {
		if src.CredentialRef == "" && ok {
			updated, err := s.sources.Update(id, source.UpdateInput{CredentialRef: &ref})
			if err != nil {
				return nil, "", false, err
			}
			return updated, ref, true, nil
		}
		if src.CredentialRef != "" {
			return src, src.CredentialRef, true, nil
		}
		return src, "", false, nil
	} else if !errors.Is(err, source.ErrNotFound) {
		return nil, "", false, err
	}
	created, err := s.sources.Create(source.CreateInput{
		Forge: forge, CloneURL: normalized, DefaultBranch: watch, CredentialRef: ref,
	})
	if err != nil {
		return nil, "", false, err
	}
	return &created.Source, ref, ok, nil
}

// routingRoutesFor is the routing lookup with a missing store degraded to
// "no hints".
func (s *Server) routingRoutesFor(repository string) []source.Route {
	if s.routing == nil {
		return nil
	}
	return s.routing.RoutesFor(repository)
}

// handleAppSourceReset serves POST .../source/reset-watermark:
// `trigger reset` clears a ref's watermark so a force-pushed head can be
// admitted again. The ref must be typed back by the caller (the UI asks
// for it) — the destructive act is confirmed twice.
func (s *Server) handleAppSourceReset(w http.ResponseWriter, r *http.Request, serverName, appName string) {
	if !s.requireGitSource(w, r, "Resetting the watermark", true) {
		return
	}
	var body struct {
		Ref     string `json:"ref"`
		Confirm string `json:"confirm"`
	}
	if err := strictDecode(r, &body); err != nil {
		writeError(w, "invalid request body")
		return
	}
	if !gitRefPattern.MatchString(body.Ref) {
		writeError(w, "ref must look like refs/heads/<branch>")
		return
	}
	if body.Confirm != body.Ref {
		writeError(w, "type the ref back to confirm the reset")
		return
	}
	args := append([]string{"trigger", "reset", "--ref", body.Ref, "--json"}, s.requestActorArgs(r)...)
	res, err := s.cliAppRun(r.Context(), serverName, appName, args...)
	if err != nil {
		writeGitSourceCLIError(w, res, err)
		return
	}
	writeRawJSON(w, res.Stdout)
}

// handleAppSourceRetry serves POST .../source/retry: `trigger retry`
// clears a failed commit's retry budget and polls immediately.
func (s *Server) handleAppSourceRetry(w http.ResponseWriter, r *http.Request, serverName, appName string) {
	if !s.requireGitSource(w, r, "Retrying the failed commit", true) {
		return
	}
	args := append([]string{"trigger", "retry", "--json"}, s.requestActorArgs(r)...)
	res, err := s.cliAppRun(r.Context(), serverName, appName, args...)
	if err != nil {
		writeGitSourceCLIError(w, res, err)
		return
	}
	writeRawJSON(w, res.Stdout)
}

// handleAppSourceInstallTimer serves POST .../source/install-timer:
// `trigger install-timer` installs the server's minutely systemd timer
// (host-scoped — no --app). The doctor-derived timer cache is dropped so
// the next read sees the new state.
func (s *Server) handleAppSourceInstallTimer(w http.ResponseWriter, r *http.Request, serverName string) {
	if !s.requireGitSource(w, r, "Installing the server poll timer", true) {
		return
	}
	srv, err := s.lookupServerStrict(r.Context(), serverName)
	if err != nil {
		writeError(w, err.Error())
		return
	}
	args := []string{"trigger", "install-timer", "--host", srv.Host}
	if srv.User != "" {
		args = append(args, "--user", srv.User)
	}
	args = append(args, "--json")
	args = append(args, s.requestActorArgs(r)...)
	res, err := s.runCLI(r.Context(), args...)
	if err != nil {
		writeGitSourceCLIError(w, res, err)
		return
	}
	s.gitTimerMu.Lock()
	delete(s.gitTimer, serverName)
	s.gitTimerMu.Unlock()
	writeRawJSON(w, res.Stdout)
}

// relaySourcePoll is the CLI call an authenticated, verified push runs to
// wake the server's admitter: `trigger poll --app X --host S --now --hint
// <sha> --json`. The server re-reads the branch head itself; the hint only
// saves an ls-remote when it matches. The raw CLI result is returned so
// callers can read a refusal's machine error.
func (s *Server) relaySourcePoll(ctx context.Context, serverName, appName, hint string) (*cli.Result, error) {
	args := []string{"trigger", "poll", "--now", "--json"}
	if hint != "" {
		args = append(args, "--hint", hint)
	}
	args = append(args, s.relayActorArgs()...)
	return s.cliAppRun(ctx, serverName, appName, args...)
}

// relayActorArgs names the relay in the app's event history.
func (s *Server) relayActorArgs() []string {
	if !s.capabilities(context.Background()).Features.ActorFlag {
		return nil
	}
	return []string{"--actor", "dash:webhook-relay"}
}

// gitPollResult mirrors the CLI's trigger-poll envelope (contracts/schema/
// trigger-poll-envelope.schema.json).
type gitPollResult struct {
	App             string `json:"app"`
	Ref             string `json:"ref"`
	Head            string `json:"head"`
	Admitted        bool   `json:"admitted"`
	Reason          string `json:"reason"`
	DeployedRelease string `json:"deployed_release"`
}

func parseGitPollResult(raw string) (*gitPollResult, error) {
	var result gitPollResult
	if err := json.Unmarshal([]byte(strings.TrimSpace(raw)), &result); err != nil {
		return nil, fmt.Errorf("poll result unreadable: %w", err)
	}
	return &result, nil
}

// canRelayGitSources reports whether the installed CLI can run the relay:
// config homes (the re-verification read) and the poll itself.
func (s *Server) canRelayGitSources() bool {
	features := s.capabilities(context.Background()).Features
	return features.ConfigHome && features.TriggerPoll && s.cliInstalled()
}

// relayVerifiedPush is the D4 webhook relay: an authenticated, verified
// push wakes the connected apps' admitter. It answers the forge as soon as
// the wake is claimed (a poll that deploys takes minutes; forges time out
// around ten seconds and redeliver), then runs the polls in the
// background and records the RESULT in the delivery ledger. No dash
// outbox: a wake lost to a crash is caught by the server's own poll
// timer. Callers hold sourceActionMu; the goroutine must not.
func (s *Server) relayVerifiedPush(w http.ResponseWriter, src *source.Source, delivery source.Delivery) {
	if !s.canRelayGitSources() {
		s.recordIgnoredDelivery(w, src.ID, delivery, noAppConnectedReason)
		return
	}
	routes := s.routingRoutesFor(src.CloneURL)
	if len(routes) == 0 {
		s.recordIgnoredDelivery(w, src.ID, delivery, noAppConnectedReason)
		return
	}
	s.relayMu.Lock()
	if s.relayInFlight[delivery.ID] {
		s.relayMu.Unlock()
		writeDeliveryStatus(w, http.StatusOK, "duplicate", "relay already running for this delivery")
		return
	}
	s.relayInFlight[delivery.ID] = true
	s.relayMu.Unlock()

	writeDeliveryStatus(w, http.StatusOK, "relayed",
		fmt.Sprintf("poll started for %s (%s)", routes[0].App, routes[0].Server))

	go func() {
		defer func() {
			s.relayMu.Lock()
			delete(s.relayInFlight, delivery.ID)
			s.relayMu.Unlock()
		}()
		delivery.Disposition = source.DispositionIgnored
		delivery.Reason = ""
		for _, route := range routes {
			reason, deployed := s.relayOneRoute(route, src, delivery.Branch, delivery.Commit)
			if reason == "" {
				continue
			}
			if delivery.Reason != "" {
				delivery.Reason += "; "
			}
			delivery.Reason += reason
			if deployed {
				delivery.Disposition = source.DispositionAdmitted
			}
		}
		if delivery.Reason == "" {
			delivery.Reason = noAppConnectedReason
		}
		if len(delivery.Reason) > 512 {
			delivery.Reason = delivery.Reason[:512]
		}
		if err := s.sources.RecordDelivery(src.ID, delivery); err != nil {
			log.Printf("[source] relay ledger record for %s: %v", src.ID, err)
			return
		}
		if delivery.Disposition == source.DispositionAdmitted {
			s.fleet.invalidate()
		}
	}()
}

// relayOneRoute verifies one routing hint against the app's live config
// and, when it matches, runs the poll. It reports the outcome reason for
// the ledger ("" = nothing to record for this route) and whether the poll
// deployed. Stale hints are unlinked; a verification that could not say
// keeps the hint (transient read, the timer still catches up).
func (s *Server) relayOneRoute(route source.Route, src *source.Source, branch, commit string) (string, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	repository, err := source.CanonicalURL(src.CloneURL)
	if err != nil {
		return "relay failed: source identity unreadable", false
	}
	match, stale := s.gitAppMatch(ctx, route.Server, route.App, repository, branch)
	if stale {
		s.noteSourceRoute(route.Server, route.App, "")
		return "", false
	}
	if !match {
		// A read that could not say, or a push to a branch this app does
		// not build: not this delivery's app. Another candidate may still
		// relay.
		return "", false
	}
	res, err := s.relaySourcePoll(ctx, route.Server, route.App, commit)
	if err != nil {
		var exited *cli.ExitStatusError
		if errors.As(err, &exited) {
			_, detail := cliMachineError(res)
			if detail == "" {
				detail = strings.TrimSpace(res.Stderr)
			}
			return fmt.Sprintf("%s on %s: poll refused: %s", route.App, route.Server, detail), false
		}
		return fmt.Sprintf("%s on %s: relay failed: %v", route.App, route.Server, err), false
	}
	result, err := parseGitPollResult(res.Stdout)
	if err != nil {
		return fmt.Sprintf("%s on %s: relay failed: %v", route.App, route.Server, err), false
	}
	if result.Admitted {
		release := result.DeployedRelease
		if release == "" {
			release = result.Head
		}
		return fmt.Sprintf("%s on %s: deployed %s", route.App, route.Server, release), true
	}
	reason := result.Reason
	if reason == "" {
		reason = "no change"
	}
	return fmt.Sprintf("%s on %s: %s", route.App, route.Server, reason), false
}

// recordIgnoredDelivery is the pre-relay ignore path: durably record the
// delivery, then acknowledge it (the C02 ack-after-fsync discipline).
func (s *Server) recordIgnoredDelivery(w http.ResponseWriter, id string, delivery source.Delivery, reason string) {
	delivery.Disposition = source.DispositionIgnored
	delivery.Reason = reason
	if err := s.sources.RecordDelivery(id, delivery); err != nil {
		writeError(w, err.Error())
		return
	}
	writeDeliveryStatus(w, http.StatusOK, "ignored", reason)
}

// noAppConnectedReason is the ledger/response reason a verified push found
// no app to wake (the pre-D4 behavior, kept verbatim).
const noAppConnectedReason = "no app is connected to this repository yet"

// gitAppMatch verifies one routing hint against the app's live config:
// Teploy home, the same repository (any spelling), the delivered branch.
// ok is false when the hint is stale (the relay unlinks it) or the read
// could not say (recorded, retried by nothing — the timer catches up).
func (s *Server) gitAppMatch(ctx context.Context, serverName, appName, repository, branch string) (match bool, stale bool) {
	home, _, _, content, ok := s.cliConfigHome(ctx, serverName, appName)
	if !ok {
		return false, false
	}
	if home != "teploy" {
		return false, true
	}
	configRepo, configBranch, hasGit, err := configkeys.GitSource([]byte(content))
	if err != nil || !hasGit {
		return false, true
	}
	canonical, err := source.CanonicalURL(configRepo)
	if err != nil || canonical != repository {
		return false, true
	}
	return configBranch == branch, false
}
