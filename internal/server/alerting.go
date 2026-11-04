package server

// alerting.go — Alerts v2 (plan P3, Slice 3): the notification channel list
// and its API, test-send, the public Dash URL, first-enable summary,
// suppression holds, silences, channel health, operation-failure incidents,
// the push endpoint, the Dash self-heartbeat and the audit trail.
//
// Every record type here is file-store only (plan principle 9):
// ~/.teploy/notifications.json (channels, public URL, heartbeats; 0600),
// <data>/incidents.json, <data>/silences.json, <data>/audit.jsonl.

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/useteploy/teploy-dash/internal/alert"
	"github.com/useteploy/teploy-dash/internal/audit"
	"github.com/useteploy/teploy-dash/internal/durable"
	"github.com/useteploy/teploy-dash/internal/heartbeat"
	"github.com/useteploy/teploy-dash/internal/incident"
	"github.com/useteploy/teploy-dash/internal/monitor"
	"github.com/useteploy/teploy-dash/internal/operation"
	"github.com/useteploy/teploy-dash/internal/outbox"
)

// alertHoldGrace is how long a hold outlives the condition that caused it
// (a deploy that just finished, maintenance just switched off): containers
// need a moment to answer health checks again.
const alertHoldGrace = 2 * time.Minute

// lockHoldCap bounds how long a deploy lock alone can hold alerts: a lock
// left behind by a crashed CLI must not hide an outage forever.
const lockHoldCap = 30 * time.Minute

// operationAutoResolve closes a one-shot operation-failure incident when no
// further attempt of the same kind on the same app follows.
const operationAutoResolve = 24 * time.Hour

type alertingState struct {
	tracker   *incident.Tracker
	initErr   error
	silences  *silenceStore
	audit     *audit.Log
	heartbeat *heartbeat.Runner
	pushLimit *rateLimiter

	holdMu    sync.Mutex
	lockFirst map[string]time.Time // first time a deploy lock was observed
	flagLast  map[string]time.Time // last time maintenance/lock was observed
}

func (s *Server) initAlerting(dataDir string) {
	a := &s.alerts
	a.lockFirst = map[string]time.Time{}
	a.flagLast = map[string]time.Time{}
	a.pushLimit = newRateLimiter()
	var errs []error
	var err error
	if a.audit, err = audit.Open(dataDir); err != nil {
		errs = append(errs, fmt.Errorf("audit log: %w", err))
		a.audit, _ = audit.Open("")
	}
	if a.silences, err = openSilences(dataDir); err != nil {
		errs = append(errs, fmt.Errorf("silences: %w", err))
		a.silences, _ = openSilences("")
	}
	opts := incident.Options{Dir: dataDir, HasChannels: s.alertChannelsConfigured, Hold: s.alertHold,
		Silenced: s.alertSilenced, PublicURL: s.alertPublicURL}
	if a.tracker, err = incident.New(opts); err != nil {
		errs = append(errs, fmt.Errorf("incidents: %w", err))
		opts.Dir = ""
		a.tracker, _ = incident.New(opts)
	}
	a.heartbeat = heartbeat.New(loadNotificationsConfig)
	a.initErr = errors.Join(errs...)
}

// AlertTracker is the shared incident tracker (monitor and restore runners
// feed it).
func (s *Server) AlertTracker() *incident.Tracker { return s.alerts.tracker }

// AlertingInitErr reports an unreadable incidents/silences/audit file.
func (s *Server) AlertingInitErr() error { return s.alerts.initErr }

// SetOutbox installs the durable alert outbox: the tracker sends through
// it and the API reads delivery state from it.
func (s *Server) SetOutbox(ob *outbox.Outbox) {
	s.outbox = ob
	if s.alerts.tracker != nil {
		s.alerts.tracker.SetAlerter(ob)
	}
}

// RunAlerting migrates a legacy notifications file, announces incidents that
// opened while no channel was configured, runs the self-heartbeat and
// sweeps one-shot incidents until ctx ends.
func (s *Server) RunAlerting(ctx context.Context) {
	if err := migrateNotificationsFile(); err != nil {
		log.Printf("[alerts] notifications migration: %v", err)
	}
	if s.alerts.tracker != nil {
		s.alerts.tracker.AnnounceUnannounced()
	}
	var wg sync.WaitGroup
	defer wg.Wait() // the heartbeat loop ends with ctx; join it before returning
	if s.alerts.heartbeat != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.alerts.heartbeat.Run(ctx)
		}()
	}
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if s.alerts.tracker != nil {
				s.alerts.tracker.Sweep()
			}
		}
	}
}

// ── notifications.json ───────────────────────────────────────────────────

func notificationsFilePath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".teploy", "notifications.json")
}

// LoadNotificationsConfig reads alert configuration from ~/.teploy/notifications.json.
// R14: an unreadable or corrupt file is an ERROR, not "notifications not
// configured" — the caller decides how to degrade; a silent empty config
// let a later partial overwrite destroy stored secrets. The returned config
// is migrated in memory (legacy fields become channels webhook/email).
func LoadNotificationsConfig() (alert.Config, error) {
	return loadNotificationsConfig()
}

func loadNotificationsConfig() (alert.Config, error) {
	raw, err := os.ReadFile(notificationsFilePath())
	if errors.Is(err, os.ErrNotExist) {
		return alert.Config{}, nil
	}
	if err != nil {
		return alert.Config{}, fmt.Errorf("read notifications config: %w", err)
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return alert.Config{}, nil
	}
	var cfg alert.Config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return alert.Config{}, fmt.Errorf("decode notifications config: %w", err)
	}
	return cfg.Migrate(), nil
}

func saveNotificationsConfig(cfg alert.Config) error {
	path := notificationsFilePath()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(cfg.Migrate(), "", "  ")
	if err != nil {
		return err
	}
	// 0600 + atomic: the file holds SMTP passwords, signing secrets and
	// tokens, and a partial write must never replace a complete config (A32).
	return atomicFileWrite(path, raw, 0600)
}

// migrateNotificationsFile rewrites a legacy file in the channel format once,
// so the stored shape matches what Dash reads.
func migrateNotificationsFile() error {
	notificationsMu.Lock()
	defer notificationsMu.Unlock()
	raw, err := os.ReadFile(notificationsFilePath())
	if errors.Is(err, os.ErrNotExist) || len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	if err != nil {
		return err
	}
	var cfg alert.Config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return err
	}
	if !cfg.HasLegacy() {
		return nil
	}
	log.Printf("[alerts] migrating notifications.json to the channel list (ids webhook/email)")
	return saveNotificationsConfig(cfg)
}

// notificationsMu serializes the notification config's read-merge-validate-
// save-publication sequence (R14): concurrent patches previously overwrote
// one another because each read the file outside any lock.
var notificationsMu sync.Mutex

// atomicFileWrite writes data to path via a same-directory temp file + rename
// so a failed or partial write cannot replace the previous complete file.
// R15: the replacement shares the audited durable semantics (unique temp,
// file sync, rename, PARENT-DIRECTORY sync after the rename).
func atomicFileWrite(path string, data []byte, mode os.FileMode) error {
	return durable.Replace(path, data, mode)
}

func (s *Server) alertChannelsConfigured() bool {
	cfg, err := loadNotificationsConfig()
	if err != nil {
		// Unreadable config: treat as configured so incidents are announced
		// (the outbox enqueues and surfaces the config error loudly).
		return true
	}
	return len(alert.ConfiguredChannels(cfg)) > 0
}

func (s *Server) alertPublicURL() string {
	cfg, err := loadNotificationsConfig()
	if err != nil {
		return ""
	}
	return strings.TrimRight(cfg.PublicURL, "/")
}

// updateNotifications runs one locked read-modify-validate-save. mutate
// returns the HTTP status for a refusal. After a save that took the config
// from zero to at least one usable channel, the first-enable summary runs.
func (s *Server) updateNotifications(mutate func(cfg *alert.Config) (int, error)) (alert.Config, int, error) {
	notificationsMu.Lock()
	defer notificationsMu.Unlock()
	cfg, err := loadNotificationsConfig()
	if err != nil {
		return cfg, http.StatusServiceUnavailable, fmt.Errorf("notification configuration unavailable: %w", err)
	}
	before := len(alert.ConfiguredChannels(cfg))
	if code, err := mutate(&cfg); err != nil {
		return cfg, code, err
	}
	if err := saveNotificationsConfig(cfg); err != nil {
		return cfg, http.StatusInternalServerError, err
	}
	if before == 0 && len(alert.ConfiguredChannels(cfg)) > 0 && s.alerts.tracker != nil {
		// First channel: one summary of what is already open, not an alert
		// per incident (plan P3.5).
		go s.alerts.tracker.AnnounceUnannounced()
	}
	return cfg, http.StatusOK, nil
}

// ── audit ────────────────────────────────────────────────────────────────

func requestActor(r *http.Request) string {
	if a := actorFromRequest(r); a != nil {
		if a.Label != "" {
			return a.Label
		}
		return a.Subject
	}
	return "local"
}

func (s *Server) auditf(r *http.Request, action, target, format string, args ...any) {
	if s.alerts.audit == nil {
		return
	}
	s.alerts.audit.Append(audit.Entry{Actor: requestActor(r), Action: action, Target: target, Detail: fmt.Sprintf(format, args...)})
}

func (s *Server) handleAudit(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var entries []audit.Entry
	if s.alerts.audit != nil {
		entries = s.alerts.audit.Recent(200)
	}
	writeData(w, entries)
}

// ── views ────────────────────────────────────────────────────────────────

type channelView struct {
	ID                string                `json:"id"`
	Type              string                `json:"type"`
	Name              string                `json:"name,omitempty"`
	URL               string                `json:"url,omitempty"`
	URLSet            bool                  `json:"url_set"`
	URLHint           string                `json:"url_hint,omitempty"`
	SecretSet         bool                  `json:"secret_set"`
	TokenSet          bool                  `json:"token_set"`
	Kinds             []string              `json:"kinds"`
	Usable            bool                  `json:"usable"`
	SMTPHost          string                `json:"smtp_host,omitempty"`
	SMTPPort          int                   `json:"smtp_port,omitempty"`
	SMTPUser          string                `json:"smtp_user,omitempty"`
	SMTPPassSet       bool                  `json:"smtp_pass_set"`
	EmailTo           string                `json:"email_to,omitempty"`
	EmailFrom         string                `json:"email_from,omitempty"`
	SMTPAllowInsecure bool                  `json:"smtp_allow_insecure,omitempty"`
	Health            *outbox.ChannelHealth `json:"health,omitempty"`
}

// viewChannel never returns a secret: signing secrets, tokens and SMTP
// passwords become *_set flags, and a URL that is itself the credential
// (Slack-compatible, Discord) is shown only as a hint.
func viewChannel(ch alert.Channel, health map[string]outbox.ChannelHealth) channelView {
	v := channelView{ID: ch.ID, Type: ch.Type, Name: ch.Name, URLSet: ch.URL != "", SecretSet: ch.Secret != "",
		TokenSet: ch.Token != "", Kinds: append([]string{}, ch.Kinds...), Usable: ch.Usable(),
		SMTPHost: ch.SMTPHost, SMTPPort: ch.SMTPPort, SMTPUser: ch.SMTPUser, SMTPPassSet: ch.SMTPPass != "",
		EmailTo: ch.EmailTo, EmailFrom: ch.EmailFrom, SMTPAllowInsecure: ch.SMTPAllowInsecure}
	if alert.URLIsCredential(ch.Type) {
		v.URLHint = alert.URLHint(ch.URL)
	} else {
		v.URL = ch.URL
	}
	if h, ok := health[ch.ID]; ok {
		v.Health = &h
	}
	return v
}

type heartbeatView struct {
	ID      string            `json:"id"`
	Name    string            `json:"name,omitempty"`
	URLSet  bool              `json:"url_set"`
	URLHint string            `json:"url_hint,omitempty"`
	Last    *heartbeat.Result `json:"last,omitempty"`
	Warning string            `json:"warning,omitempty"`
}

func (s *Server) viewHeartbeat(ctx context.Context, hb alert.HeartbeatTarget, results map[string]heartbeat.Result) heartbeatView {
	v := heartbeatView{ID: hb.ID, Name: hb.Name, URLSet: hb.URL != "", URLHint: alert.URLHint(hb.URL),
		Warning: heartbeat.SelfHostedWarning(ctx, hb.URL)}
	if r, ok := results[hb.ID]; ok {
		v.Last = &r
	}
	return v
}

func (s *Server) notificationsView(ctx context.Context, cfg alert.Config) map[string]any {
	var health map[string]outbox.ChannelHealth
	if s.outbox != nil {
		health = s.outbox.ChannelHealth()
	}
	channels := make([]channelView, 0, len(cfg.Channels))
	for _, ch := range cfg.Channels {
		channels = append(channels, viewChannel(ch, health))
	}
	var results map[string]heartbeat.Result
	if s.alerts.heartbeat != nil {
		results = s.alerts.heartbeat.Results()
	}
	beats := make([]heartbeatView, 0, len(cfg.Heartbeats))
	for _, hb := range cfg.Heartbeats {
		beats = append(beats, s.viewHeartbeat(ctx, hb, results))
	}
	view := map[string]any{
		"channels":                   channels,
		"public_url":                 cfg.PublicURL,
		"heartbeats":                 beats,
		"heartbeat_interval_minutes": int(heartbeat.Interval(cfg) / time.Minute),
		// Legacy flat view of channels "webhook"/"email" for older clients.
		"webhook_secret_set": false,
		"smtp_pass_set":      false,
	}
	if wh, ok := cfg.Channel(alert.ChannelWebhook); ok && wh.Type == alert.TypeWebhook {
		view["webhook_url"] = wh.URL
		view["webhook_secret_set"] = wh.Secret != ""
	}
	if em, ok := cfg.Channel(alert.ChannelEmail); ok && em.Type == alert.TypeEmail {
		view["smtp_host"], view["smtp_port"], view["smtp_user"] = em.SMTPHost, em.SMTPPort, em.SMTPUser
		view["smtp_pass_set"] = em.SMTPPass != ""
		view["email_to"], view["email_from"], view["smtp_allow_insecure"] = em.EmailTo, em.EmailFrom, em.SMTPAllowInsecure
	}
	return view
}

// ── /api/notifications ───────────────────────────────────────────────────

func upsertChannel(cfg *alert.Config, ch alert.Channel) {
	for i := range cfg.Channels {
		if cfg.Channels[i].ID == ch.ID {
			cfg.Channels[i] = ch
			return
		}
	}
	cfg.Channels = append(cfg.Channels, ch)
}

func removeChannel(cfg *alert.Config, id string) bool {
	for i := range cfg.Channels {
		if cfg.Channels[i].ID == id {
			cfg.Channels = append(cfg.Channels[:i], cfg.Channels[i+1:]...)
			return true
		}
	}
	return false
}

func (s *Server) handleNotifications(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	switch r.Method {
	case http.MethodGet:
		cfg, err := loadNotificationsConfig()
		if err != nil {
			// R14: corrupt/unreadable state is a configuration error, not a
			// silently empty one.
			writeErrorStatus(w, "notification configuration unavailable: "+err.Error(), http.StatusServiceUnavailable)
			return
		}
		writeData(w, s.notificationsView(r.Context(), cfg))
	case http.MethodPost:
		// Patch DTO (A34): absent = preserve; present = replace (empty string
		// clears deliberately). The flat webhook_*/smtp_* fields edit the
		// channels with ids "webhook" and "email" (pre-v2 clients).
		var patch struct {
			WebhookURL               *string `json:"webhook_url"`
			WebhookSecret            *string `json:"webhook_secret"`
			SMTPHost                 *string `json:"smtp_host"`
			SMTPPort                 *int    `json:"smtp_port"`
			SMTPUser                 *string `json:"smtp_user"`
			SMTPPass                 *string `json:"smtp_pass"`
			EmailTo                  *string `json:"email_to"`
			EmailFrom                *string `json:"email_from"`
			SMTPAllowInsecure        *bool   `json:"smtp_allow_insecure"`
			PublicURL                *string `json:"public_url"`
			HeartbeatIntervalMinutes *int    `json:"heartbeat_interval_minutes"`
		}
		if err := strictDecode(r, &patch); err != nil {
			writeError(w, "invalid request body")
			return
		}
		_, code, err := s.updateNotifications(func(cfg *alert.Config) (int, error) {
			if patch.PublicURL != nil {
				u := strings.TrimRight(strings.TrimSpace(*patch.PublicURL), "/")
				if u != "" {
					if err := alert.ValidateURL(u); err != nil {
						return http.StatusBadRequest, fmt.Errorf("public URL: %w", err)
					}
				}
				cfg.PublicURL = u
			}
			if patch.HeartbeatIntervalMinutes != nil {
				if m := *patch.HeartbeatIntervalMinutes; m < 0 || m > 60 {
					return http.StatusBadRequest, fmt.Errorf("heartbeat interval must be 1-60 minutes")
				}
				cfg.HeartbeatIntervalMinutes = *patch.HeartbeatIntervalMinutes
			}
			if patch.WebhookURL != nil || patch.WebhookSecret != nil {
				wh, _ := cfg.Channel(alert.ChannelWebhook)
				if wh.ID == "" {
					wh = alert.Channel{ID: alert.ChannelWebhook, Type: alert.TypeWebhook, Name: "Webhook"}
				}
				if wh.Type != alert.TypeWebhook {
					return http.StatusConflict, fmt.Errorf("channel %q is not a webhook; edit it in the channel list", wh.ID)
				}
				if patch.WebhookURL != nil {
					wh.URL = strings.TrimSpace(*patch.WebhookURL)
				}
				if patch.WebhookSecret != nil {
					wh.Secret = strings.TrimSpace(*patch.WebhookSecret)
				}
				if wh.URL == "" {
					removeChannel(cfg, wh.ID)
				} else {
					if err := alert.ValidateURL(wh.URL); err != nil {
						return http.StatusBadRequest, err
					}
					upsertChannel(cfg, wh)
				}
			}
			if patch.SMTPHost != nil || patch.SMTPPort != nil || patch.SMTPUser != nil || patch.SMTPPass != nil ||
				patch.EmailTo != nil || patch.EmailFrom != nil || patch.SMTPAllowInsecure != nil {
				em, _ := cfg.Channel(alert.ChannelEmail)
				if em.ID == "" {
					em = alert.Channel{ID: alert.ChannelEmail, Type: alert.TypeEmail, Name: "Email"}
				}
				if em.Type != alert.TypeEmail {
					return http.StatusConflict, fmt.Errorf("channel %q is not an email channel", em.ID)
				}
				if patch.SMTPHost != nil {
					em.SMTPHost = strings.TrimSpace(*patch.SMTPHost)
				}
				if patch.SMTPPort != nil {
					em.SMTPPort = *patch.SMTPPort
				}
				if patch.SMTPUser != nil {
					em.SMTPUser = *patch.SMTPUser
				}
				if patch.SMTPPass != nil {
					em.SMTPPass = *patch.SMTPPass
				}
				if patch.EmailTo != nil {
					em.EmailTo = *patch.EmailTo
				}
				if patch.EmailFrom != nil {
					em.EmailFrom = *patch.EmailFrom
				}
				if patch.SMTPAllowInsecure != nil {
					em.SMTPAllowInsecure = *patch.SMTPAllowInsecure
				}
				if em.SMTPHost == "" && em.EmailTo == "" && em.SMTPUser == "" && em.SMTPPass == "" && em.EmailFrom == "" {
					removeChannel(cfg, em.ID)
				} else {
					upsertChannel(cfg, em)
				}
			}
			return 0, nil
		})
		if err != nil {
			writeErrorStatus(w, err.Error(), code)
			return
		}
		s.auditf(r, "notifications.update", "", "settings patch")
		writeData(w, map[string]bool{"saved": true})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// handleNotificationAction serves /api/notifications/channels/{id}[/test]
// and /api/notifications/heartbeats/{id}[/test].
func (s *Server) handleNotificationAction(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	parts := strings.Split(strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/notifications/"), "/"), "/")
	if len(parts) < 2 || len(parts) > 3 || !alert.ValidChannelID(parts[1]) || (len(parts) == 3 && parts[2] != "test") {
		jsonError(w, "not found", http.StatusNotFound)
		return
	}
	id, test := parts[1], len(parts) == 3
	switch parts[0] {
	case "channels":
		if test {
			s.testChannel(w, r, id)
			return
		}
		s.channelCRUD(w, r, id)
	case "heartbeats":
		if test {
			s.testHeartbeat(w, r, id)
			return
		}
		s.heartbeatCRUD(w, r, id)
	default:
		jsonError(w, "not found", http.StatusNotFound)
	}
}

type channelPatch struct {
	Type              *string   `json:"type"`
	Name              *string   `json:"name"`
	URL               *string   `json:"url"`
	Secret            *string   `json:"secret"`
	Token             *string   `json:"token"`
	Kinds             *[]string `json:"kinds"`
	SMTPHost          *string   `json:"smtp_host"`
	SMTPPort          *int      `json:"smtp_port"`
	SMTPUser          *string   `json:"smtp_user"`
	SMTPPass          *string   `json:"smtp_pass"`
	EmailTo           *string   `json:"email_to"`
	EmailFrom         *string   `json:"email_from"`
	SMTPAllowInsecure *bool     `json:"smtp_allow_insecure"`
}

func (p channelPatch) apply(ch *alert.Channel) {
	set := func(dst *string, v *string, trim bool) {
		if v != nil {
			if trim {
				*dst = strings.TrimSpace(*v)
			} else {
				*dst = *v
			}
		}
	}
	set(&ch.Name, p.Name, true)
	set(&ch.URL, p.URL, true)
	set(&ch.Secret, p.Secret, true)
	set(&ch.Token, p.Token, true)
	set(&ch.SMTPHost, p.SMTPHost, true)
	set(&ch.SMTPUser, p.SMTPUser, false)
	set(&ch.SMTPPass, p.SMTPPass, false)
	set(&ch.EmailTo, p.EmailTo, true)
	set(&ch.EmailFrom, p.EmailFrom, true)
	if p.SMTPPort != nil {
		ch.SMTPPort = *p.SMTPPort
	}
	if p.SMTPAllowInsecure != nil {
		ch.SMTPAllowInsecure = *p.SMTPAllowInsecure
	}
	if p.Kinds != nil {
		ch.Kinds = append([]string(nil), (*p.Kinds)...)
	}
}

func (s *Server) channelCRUD(w http.ResponseWriter, r *http.Request, id string) {
	switch r.Method {
	case http.MethodPut:
		var patch channelPatch
		if err := strictDecode(r, &patch); err != nil {
			writeError(w, "invalid request body")
			return
		}
		created := false
		cfg, code, err := s.updateNotifications(func(cfg *alert.Config) (int, error) {
			ch, exists := cfg.Channel(id)
			if !exists {
				if patch.Type == nil {
					return http.StatusBadRequest, fmt.Errorf("type is required for a new channel")
				}
				ch = alert.Channel{ID: id, Type: *patch.Type}
				created = true
			} else if patch.Type != nil && *patch.Type != ch.Type {
				// The ledger is keyed by channel id: a type change under the
				// same id would replay one channel's history at another.
				return http.StatusConflict, fmt.Errorf("a channel's type cannot change; delete it and add a new one")
			}
			patch.apply(&ch)
			if err := alert.ValidateChannel(ch); err != nil {
				return http.StatusBadRequest, err
			}
			upsertChannel(cfg, ch)
			return 0, nil
		})
		if err != nil {
			writeErrorStatus(w, err.Error(), code)
			return
		}
		ch, _ := cfg.Channel(id)
		action := "channel.update"
		if created {
			action = "channel.create"
		}
		s.auditf(r, action, id, "type %s", ch.Type)
		var health map[string]outbox.ChannelHealth
		if s.outbox != nil {
			health = s.outbox.ChannelHealth()
		}
		writeData(w, viewChannel(ch, health))
	case http.MethodDelete:
		removed := false
		_, code, err := s.updateNotifications(func(cfg *alert.Config) (int, error) {
			removed = removeChannel(cfg, id)
			if !removed {
				return http.StatusNotFound, fmt.Errorf("channel not found")
			}
			return 0, nil
		})
		if err != nil {
			writeErrorStatus(w, err.Error(), code)
			return
		}
		s.auditf(r, "channel.delete", id, "")
		writeData(w, map[string]bool{"deleted": true})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

// testEvent is the kind:"test" event (plan 2.5: receivers acknowledge and
// do not open incidents). status is "up" so v1 receivers do not either.
func (s *Server) testEvent() alert.Event {
	now := time.Now().UTC()
	link := ""
	if base := s.alertPublicURL(); base != "" {
		link = base + "/settings"
	}
	return alert.Event{MonitorID: "teploy-dash-test", MonitorName: "Teploy Dash", Status: "up",
		Message: "Test notification: this channel is configured correctly.", OccurredAt: now,
		Source: alert.SourceMonitor, DeliveryID: "test-" + randomHex(12), Version: 2, Kind: alert.KindTest,
		RuleID: "teploy-dash-test", Service: "teploy-dash", Severity: alert.SeverityInfo, StartedAt: now, URL: link}
}

func (s *Server) testChannel(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	cfg, err := loadNotificationsConfig()
	if err != nil {
		writeErrorStatus(w, "notification configuration unavailable: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	if _, ok := cfg.Channel(id); !ok {
		writeErrorStatus(w, "channel not found", http.StatusNotFound)
		return
	}
	sendErr := alert.New(cfg).SendChannel(s.testEvent(), id)
	result := map[string]any{"ok": sendErr == nil}
	if sendErr != nil {
		result["error"] = sendErr.Error()
	}
	s.auditf(r, "channel.test", id, "ok=%v", sendErr == nil)
	writeData(w, result)
}

func (s *Server) heartbeatCRUD(w http.ResponseWriter, r *http.Request, id string) {
	switch r.Method {
	case http.MethodPut:
		var patch struct {
			Name *string `json:"name"`
			URL  *string `json:"url"`
		}
		if err := strictDecode(r, &patch); err != nil {
			writeError(w, "invalid request body")
			return
		}
		var saved alert.HeartbeatTarget
		_, code, err := s.updateNotifications(func(cfg *alert.Config) (int, error) {
			idx := -1
			for i, hb := range cfg.Heartbeats {
				if hb.ID == id {
					idx = i
				}
			}
			hb := alert.HeartbeatTarget{ID: id}
			if idx >= 0 {
				hb = cfg.Heartbeats[idx]
			} else if len(cfg.Heartbeats) >= 10 {
				return http.StatusBadRequest, fmt.Errorf("at most 10 heartbeat receivers")
			}
			if patch.Name != nil {
				hb.Name = strings.TrimSpace(*patch.Name)
			}
			if patch.URL != nil {
				hb.URL = strings.TrimSpace(*patch.URL)
			}
			if err := alert.ValidateURL(hb.URL); err != nil {
				return http.StatusBadRequest, err
			}
			if len(hb.Name) > 80 {
				return http.StatusBadRequest, fmt.Errorf("name is too long")
			}
			if idx >= 0 {
				cfg.Heartbeats[idx] = hb
			} else {
				cfg.Heartbeats = append(cfg.Heartbeats, hb)
			}
			saved = hb
			return 0, nil
		})
		if err != nil {
			writeErrorStatus(w, err.Error(), code)
			return
		}
		s.auditf(r, "heartbeat.save", id, "%s", alert.URLHint(saved.URL))
		writeData(w, s.viewHeartbeat(r.Context(), saved, nil))
	case http.MethodDelete:
		_, code, err := s.updateNotifications(func(cfg *alert.Config) (int, error) {
			for i, hb := range cfg.Heartbeats {
				if hb.ID == id {
					cfg.Heartbeats = append(cfg.Heartbeats[:i], cfg.Heartbeats[i+1:]...)
					return 0, nil
				}
			}
			return http.StatusNotFound, fmt.Errorf("heartbeat receiver not found")
		})
		if err != nil {
			writeErrorStatus(w, err.Error(), code)
			return
		}
		if s.alerts.heartbeat != nil {
			s.alerts.heartbeat.Forget(id)
		}
		s.auditf(r, "heartbeat.delete", id, "")
		writeData(w, map[string]bool{"deleted": true})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) testHeartbeat(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	cfg, err := loadNotificationsConfig()
	if err != nil {
		writeErrorStatus(w, "notification configuration unavailable: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	for _, hb := range cfg.Heartbeats {
		if hb.ID == id {
			res := s.alerts.heartbeat.Ping(r.Context(), hb)
			s.auditf(r, "heartbeat.test", id, "ok=%v", res.OK)
			writeData(w, res)
			return
		}
	}
	writeErrorStatus(w, "heartbeat receiver not found", http.StatusNotFound)
}

// ── /api/alerts ──────────────────────────────────────────────────────────

func (s *Server) handleAlertIncidents(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	resp := map[string]any{"open": []incident.Incident{}, "held": []incident.Held{}}
	if t := s.alerts.tracker; t != nil {
		resp["open"] = t.Open()
		if h := t.HeldRules(); h != nil {
			resp["held"] = h
		}
	}
	writeData(w, resp)
}

func (s *Server) handleAlertDeliveries(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	resp := map[string]any{"deliveries": []outbox.DeliveryStatus{}, "channels": map[string]outbox.ChannelHealth{}}
	if s.outbox != nil {
		resp["deliveries"] = s.outbox.History(100)
		resp["channels"] = s.outbox.ChannelHealth()
	}
	writeData(w, resp)
}

// ── channel health (plan P3.6) ───────────────────────────────────────────

func channelRule(ch alert.Channel) incident.Rule {
	name := ch.Name
	if name == "" {
		name = ch.ID
	}
	return incident.Rule{Key: "channel:" + ch.ID, Source: alert.SourceAudit, ID: "channel-" + ch.ID,
		Name: "alert channel " + name, Target: ch.Type, Severity: alert.SeverityWarning, Path: "/settings",
		NoHold: true, Policy: incident.Policy{FailAfter: 1, RecoverAfter: 1}, ExcludeChannels: []string{ch.ID}}
}

// OnChannelDeadLetter alerts through the OTHER channels when one channel
// dead-letters a delivery (only when another usable channel exists).
func (s *Server) OnChannelDeadLetter(channel string, ev alert.Event, lastErr string) {
	t := s.alerts.tracker
	cfg, err := loadNotificationsConfig()
	if t == nil || err != nil {
		return
	}
	ch, ok := cfg.Channel(channel)
	if !ok {
		return
	}
	others := 0
	for _, id := range alert.ConfiguredChannels(cfg) {
		if id != channel {
			others++
		}
	}
	if others == 0 {
		return
	}
	t.Observe(channelRule(ch), false, time.Now(), fmt.Sprintf("deliveries to %s are failing (dead-lettered after retries): %s", channel, lastErr))
}

// OnChannelDelivered closes a channel-health incident once the channel
// delivers again.
func (s *Server) OnChannelDelivered(channel string, ev alert.Event) {
	t := s.alerts.tracker
	if t == nil || len(t.Open()) == 0 {
		return
	}
	cfg, err := loadNotificationsConfig()
	if err != nil {
		return
	}
	if ch, ok := cfg.Channel(channel); ok {
		t.Observe(channelRule(ch), true, time.Now(), "channel is delivering again")
	}
}

// ── operations (plan P3 item 10) ─────────────────────────────────────────

// alertedOperationKinds are the app operations whose failure is an
// incident. Template installs (no app yet), removes and lifecycle verbs are
// not: their failure leaves no running thing a receiver could track.
var alertedOperationKinds = map[operation.Kind]string{
	operation.KindDeploy:        "deploy",
	operation.KindRedeploy:      "redeploy",
	operation.KindRollback:      "rollback",
	operation.KindBackupCreate:  "backup",
	operation.KindBackupRestore: "backup restore",
	operation.KindMaintenance:   "maintenance",
}

// operationRule maps an operation onto its incident rule. A one-shot
// failure still follows the incident model: the rule is the op kind on the
// app (stable across attempts), it opens on a failure, recovers on the next
// success of the same kind on the same app, and otherwise auto-resolves
// (reason "retired") after operationAutoResolve, so receivers never keep a
// stale open card.
func operationRule(op *operation.Operation) (incident.Rule, bool) {
	label, ok := alertedOperationKinds[op.Request.Kind]
	if !ok || op.Request.App == "" {
		return incident.Rule{}, false
	}
	id := fmt.Sprintf("op-%s-%s-%s", op.Request.Kind, op.Request.Server, op.Request.App)
	return incident.Rule{Source: alert.SourceOperation, ID: id,
		Name: fmt.Sprintf("%s of %s on %s", label, op.Request.App, op.Request.Server),
		App:  op.Request.App, Server: op.Request.Server, Target: string(op.Request.Kind),
		Severity: alert.SeverityWarning, Path: "/operations/" + op.ID, NoHold: true,
		Policy: incident.Policy{FailAfter: 1, RecoverAfter: 1, AutoResolveAfter: operationAutoResolve}}, true
}

func (s *Server) onOperationFinished(op *operation.Operation) {
	t := s.alerts.tracker
	if t == nil || op == nil {
		return
	}
	rule, ok := operationRule(op)
	if !ok {
		return
	}
	at := time.Now()
	if op.FinishedAt != nil {
		at = *op.FinishedAt
	}
	switch op.Status {
	case operation.StatusFailed:
		msg := op.Error
		if msg == "" && op.ExitCode != nil {
			msg = fmt.Sprintf("exit code %d", *op.ExitCode)
		}
		if msg == "" {
			msg = "operation failed"
		}
		t.Observe(rule, false, at, msg)
	case operation.StatusSucceeded:
		t.Observe(rule, true, at, rule.Name+" succeeded")
	}
}

// ── holds (plan P3.2) ────────────────────────────────────────────────────

// alertHold reports whether a rule's app is in maintenance, holds a deploy
// lock, or has a queued/running Dash operation — plus alertHoldGrace after
// each ends. The operation hold is CAPPED at grace + the operation's
// timeout from its start, so a hung deploy cannot hide an outage; the lock
// hold is capped at grace + lockHoldCap from first sighting. Maintenance is
// a deliberate state and is not capped. Flags come from the fleet cache
// (refreshed by page loads and the 5-minute fleet sweep), so a flag change
// can take up to one sweep to apply.
func (s *Server) alertHold(rule incident.Rule, now time.Time) (bool, string) {
	if rule.App == "" {
		return false, ""
	}
	if s.operations != nil {
		for _, op := range s.operations.List("", "", 0) {
			if op.Request.App != rule.App || (rule.Server != "" && op.Request.Server != rule.Server) {
				continue
			}
			if held, why := operationHolds(op, now); held {
				return true, why
			}
		}
	}
	key := rule.Server + "/" + rule.App
	maint, locked, seen := false, false, false
	for _, app := range s.fleet.snapshot() {
		if app.App == rule.App && (rule.Server == "" || app.Server == rule.Server) {
			seen = true
			maint = maint || app.Maintenance
			locked = locked || app.Locked
		}
	}
	a := &s.alerts
	a.holdMu.Lock()
	defer a.holdMu.Unlock()
	if a.lockFirst == nil {
		a.lockFirst, a.flagLast = map[string]time.Time{}, map[string]time.Time{}
	}
	if seen && !locked {
		delete(a.lockFirst, key)
	}
	switch {
	case maint:
		a.flagLast[key] = now
		return true, rule.App + " is in maintenance"
	case locked:
		first, ok := a.lockFirst[key]
		if !ok {
			first = now
			a.lockFirst[key] = first
		}
		if now.Sub(first) <= lockHoldCap+alertHoldGrace {
			a.flagLast[key] = now
			return true, rule.App + " holds a deploy lock"
		}
	}
	if last, ok := a.flagLast[key]; ok && now.Sub(last) < alertHoldGrace {
		return true, "grace period after maintenance or a deploy lock on " + rule.App
	}
	return false, ""
}

// operationHolds is one operation's hold window: from admission until grace
// after it finishes, capped at grace + its kind's timeout from its start.
func operationHolds(op *operation.Operation, now time.Time) (bool, string) {
	start := op.CreatedAt
	if op.StartedAt != nil {
		start = *op.StartedAt
	}
	end := start.Add(operation.KindTimeout(op.Request.Kind) + alertHoldGrace)
	if op.Status.Terminal() {
		if op.FinishedAt == nil {
			return false, ""
		}
		if fin := op.FinishedAt.Add(alertHoldGrace); fin.Before(end) {
			end = fin
		}
	}
	if now.Before(op.CreatedAt) || !now.Before(end) {
		return false, ""
	}
	if op.Status.Terminal() {
		return true, fmt.Sprintf("grace period after %s %s", op.Request.Kind, op.ID)
	}
	return true, fmt.Sprintf("%s %s is %s", op.Request.Kind, op.ID, op.Status)
}

// ── silences ─────────────────────────────────────────────────────────────

// Silence scopes.
const (
	silenceAll     = "all"
	silenceApp     = "app"
	silenceMonitor = "monitor"
)

const maxSilence = 30 * 24 * time.Hour

// Silence mutes alert opens for a scope until it expires. Held, not
// dropped: failures keep counting and a rule still failing when the
// silence ends opens then.
type Silence struct {
	ID        string    `json:"id"`
	Scope     string    `json:"scope"`
	Target    string    `json:"target,omitempty"`
	Reason    string    `json:"reason,omitempty"`
	CreatedBy string    `json:"created_by,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
}

func (sl Silence) covers(ruleID, app string) bool {
	switch sl.Scope {
	case silenceAll:
		return true
	case silenceApp:
		return app != "" && app == sl.Target
	case silenceMonitor:
		return ruleID == sl.Target
	}
	return false
}

type silenceStore struct {
	path string
	mu   sync.Mutex
	list []Silence
}

func openSilences(dir string) (*silenceStore, error) {
	st := &silenceStore{}
	if dir == "" {
		return st, nil
	}
	st.path = filepath.Join(dir, "silences.json")
	raw, err := os.ReadFile(st.path)
	if errors.Is(err, os.ErrNotExist) {
		return st, nil
	}
	if err != nil {
		return nil, err
	}
	if len(bytes.TrimSpace(raw)) > 0 {
		if err := json.Unmarshal(raw, &st.list); err != nil {
			return nil, err
		}
	}
	return st, nil
}

func (st *silenceStore) saveLocked() error {
	if st.path == "" {
		return nil
	}
	raw, err := json.MarshalIndent(st.list, "", "  ")
	if err != nil {
		return err
	}
	return durable.Replace(st.path, raw, 0600)
}

// Active lists unexpired silences, soonest expiry first; expired ones are
// pruned.
func (st *silenceStore) Active(now time.Time) []Silence {
	st.mu.Lock()
	defer st.mu.Unlock()
	kept := st.list[:0]
	var out []Silence
	for _, sl := range st.list {
		if now.Before(sl.ExpiresAt) {
			kept = append(kept, sl)
			out = append(out, sl)
		}
	}
	if len(kept) != len(st.list) {
		st.list = kept
		if err := st.saveLocked(); err != nil {
			log.Printf("[alerts] silence prune: %v", err)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ExpiresAt.Before(out[j].ExpiresAt) })
	return out
}

func (st *silenceStore) Add(sl Silence) error {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.list = append(st.list, sl)
	return st.saveLocked()
}

func (st *silenceStore) Remove(id string) (bool, error) {
	st.mu.Lock()
	defer st.mu.Unlock()
	for i, sl := range st.list {
		if sl.ID == id {
			st.list = append(st.list[:i], st.list[i+1:]...)
			return true, st.saveLocked()
		}
	}
	return false, nil
}

func (s *Server) alertSilenced(rule incident.Rule, now time.Time) (bool, string) {
	if s.alerts.silences == nil {
		return false, ""
	}
	for _, sl := range s.alerts.silences.Active(now) {
		if sl.covers(rule.ID, rule.App) {
			why := "until " + sl.ExpiresAt.UTC().Format(time.RFC3339)
			if sl.Reason != "" {
				why += " (" + sl.Reason + ")"
			}
			return true, why
		}
	}
	return false, ""
}

func (s *Server) handleSilences(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	if s.alerts.silences == nil {
		s.alerts.silences, _ = openSilences("")
	}
	id := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/alerts/silences"), "/")
	switch {
	case id == "" && r.Method == http.MethodGet:
		writeData(w, s.alerts.silences.Active(time.Now()))
	case id == "" && r.Method == http.MethodPost:
		var body struct {
			Scope           string `json:"scope"`
			Target          string `json:"target"`
			Reason          string `json:"reason"`
			DurationMinutes int    `json:"duration_minutes"`
		}
		if err := strictDecode(r, &body); err != nil {
			writeError(w, "invalid request body")
			return
		}
		body.Target, body.Reason = strings.TrimSpace(body.Target), strings.TrimSpace(body.Reason)
		switch body.Scope {
		case silenceAll:
			body.Target = ""
		case silenceApp, silenceMonitor:
			if body.Target == "" || len(body.Target) > 128 {
				writeError(w, "an app or monitor silence needs its target")
				return
			}
		default:
			writeError(w, "scope must be all, app or monitor")
			return
		}
		d := time.Duration(body.DurationMinutes) * time.Minute
		if d < time.Minute || d > maxSilence {
			writeError(w, "duration must be between 1 minute and 30 days")
			return
		}
		if len(body.Reason) > 200 {
			writeError(w, "reason is too long")
			return
		}
		now := time.Now().UTC()
		sl := Silence{ID: "sil_" + randomHex(8), Scope: body.Scope, Target: body.Target, Reason: body.Reason,
			CreatedBy: requestActor(r), CreatedAt: now, ExpiresAt: now.Add(d)}
		if err := s.alerts.silences.Add(sl); err != nil {
			writeErrorStatus(w, err.Error(), http.StatusInternalServerError)
			return
		}
		closed := 0
		if t := s.alerts.tracker; t != nil {
			// What the silence covers closes with reason suppressed_end; a
			// rule still failing when the silence expires opens a fresh
			// incident then.
			closed = t.CloseMatching(func(inc incident.Incident) bool { return sl.covers(inc.RuleID, inc.App) },
				alert.ReasonSuppressedEnd, "silenced until "+sl.ExpiresAt.Format(time.RFC3339))
		}
		s.auditf(r, "silence.create", sl.ID, "scope=%s target=%s until=%s reason=%q closed=%d",
			sl.Scope, sl.Target, sl.ExpiresAt.Format(time.RFC3339), sl.Reason, closed)
		writeData(w, sl)
	case id != "" && r.Method == http.MethodDelete:
		ok, err := s.alerts.silences.Remove(id)
		if err != nil {
			writeErrorStatus(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if !ok {
			writeErrorStatus(w, "silence not found", http.StatusNotFound)
			return
		}
		s.auditf(r, "silence.delete", id, "")
		writeData(w, map[string]bool{"deleted": true})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// ── push endpoint (plan P3.8) ────────────────────────────────────────────

// rateLimiter is a fixed-window counter per key.
type rateLimiter struct {
	mu      sync.Mutex
	windows map[string]*rateWindow
}

type rateWindow struct {
	start time.Time
	count int
}

func newRateLimiter() *rateLimiter { return &rateLimiter{windows: map[string]*rateWindow{}} }

func (l *rateLimiter) allow(key string, limit int, window time.Duration, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.windows) > 10000 {
		for k, w := range l.windows {
			if now.Sub(w.start) > window {
				delete(l.windows, k)
			}
		}
	}
	w := l.windows[key]
	if w == nil || now.Sub(w.start) > window {
		w = &rateWindow{start: now}
		l.windows[key] = w
	}
	w.count++
	return w.count <= limit
}

// Push limits: per client address (also bounds token guessing) and per
// token (a looping job cannot flood the check history).
const (
	pushPerIPPerMinute    = 60
	pushPerTokenPerMinute = 12
)

// handlePush is GET/POST/HEAD /api/push/<token>: unauthenticated and
// capability-free by design, scoped by the monitor's secret token.
// ?status=down (or fail) records a failure the job reports; ?msg= sets the
// check message.
func (s *Server) handlePush(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	switch r.Method {
	case http.MethodGet, http.MethodPost, http.MethodHead:
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.alerts.pushLimit == nil {
		s.alerts.pushLimit = newRateLimiter()
	}
	now := time.Now()
	ip := r.RemoteAddr
	if host, _, err := net.SplitHostPort(ip); err == nil {
		ip = host
	}
	if s.gate != nil {
		ip = s.gate.clientIP(r)
	}
	token := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/push/"), "/")
	if !s.alerts.pushLimit.allow("ip:"+ip, pushPerIPPerMinute, time.Minute, now) ||
		(token != "" && !s.alerts.pushLimit.allow("tok:"+token, pushPerTokenPerMinute, time.Minute, now)) {
		w.Header().Set("Retry-After", "60")
		jsonError(w, "too many pings", http.StatusTooManyRequests)
		return
	}
	if s.monitor == nil || len(token) < 32 || len(token) > 128 {
		jsonError(w, "not found", http.StatusNotFound)
		return
	}
	q := r.URL.Query()
	ok := true
	switch strings.ToLower(q.Get("status")) {
	case "down", "fail", "failed", "error":
		ok = false
	}
	msg := q.Get("msg")
	if len(msg) > 200 {
		msg = msg[:200]
	}
	if _, err := s.monitor.Ping(token, ok, msg); err != nil {
		if errors.Is(err, monitor.ErrUnknownPushToken) {
			jsonError(w, "not found", http.StatusNotFound)
			return
		}
		jsonError(w, err.Error(), http.StatusConflict)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

// newPushToken mints a monitor's push token (64 hex chars).
func newPushToken() string { return randomHex(32) }
