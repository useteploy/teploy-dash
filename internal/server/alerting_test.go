package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/useteploy/teploy-dash/internal/alert"
	"github.com/useteploy/teploy-dash/internal/caps"
	"github.com/useteploy/teploy-dash/internal/incident"
	"github.com/useteploy/teploy-dash/internal/monitor"
	"github.com/useteploy/teploy-dash/internal/operation"
	"github.com/useteploy/teploy-dash/internal/remote"
	"github.com/useteploy/teploy-dash/internal/store"
)

type captureSink struct {
	mu  sync.Mutex
	evs []alert.Event
	sup []string
}

func (c *captureSink) Send(ev alert.Event) {
	c.mu.Lock()
	c.evs = append(c.evs, ev)
	c.mu.Unlock()
}
func (c *captureSink) RecordSuppressed(ev alert.Event, note string) {
	c.mu.Lock()
	c.sup = append(c.sup, note)
	c.mu.Unlock()
}
func (c *captureSink) events() []alert.Event {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]alert.Event(nil), c.evs...)
}

// alertServer is a bare Server with alerting initialized against a temp
// data dir and a temp HOME (notifications.json).
func alertServer(t *testing.T) (*Server, *captureSink) {
	t.Helper()
	withTempGroupsFile(t)
	s := &Server{store: store.NewFileStore(t.TempDir()), fleet: &fleetCache{ttl: time.Minute}}
	s.initAlerting(t.TempDir())
	s.alerts.tracker.EndAdoption() // fresh data dir: nothing to adopt
	sink := &captureSink{}
	s.alerts.tracker.SetAlerter(sink)
	return s, sink
}

func call(t *testing.T, h http.HandlerFunc, method, path, body string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req = withUser(req, &sessionInfo{sub: "tyler", user: "tyler", role: RoleAdmin, local: true, caps: caps.PresetForRole(RoleAdmin)})
	w := httptest.NewRecorder()
	h(w, req)
	var out map[string]any
	json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

func writeNotifications(t *testing.T, raw string) {
	t.Helper()
	if err := os.WriteFile(notificationsFilePath(), []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
}

// A legacy notifications.json migrates to channels webhook/email: GET shows
// them, the file is rewritten once in the channel format, stays 0600, and
// keeps every secret.
func TestLegacyNotificationsMigrate(t *testing.T) {
	s, _ := alertServer(t)
	writeNotifications(t, `{"webhook_url":"https://hooks.example/a","webhook_secret":"sig","smtp_host":"smtp.example","smtp_port":587,"smtp_user":"u","smtp_pass":"p","email_to":"ops@example","email_from":"dash@example"}`)
	code, body := call(t, s.handleNotifications, "GET", "/api/notifications", "")
	if code != 200 {
		t.Fatalf("GET %d", code)
	}
	data := body["data"].(map[string]any)
	chans := data["channels"].([]any)
	if len(chans) != 2 || chans[0].(map[string]any)["id"] != "webhook" || chans[1].(map[string]any)["id"] != "email" {
		t.Fatalf("channels = %v", chans)
	}
	raw, _ := json.Marshal(data)
	if strings.Contains(string(raw), `"sig"`) || strings.Contains(string(raw), `"p"`) {
		t.Fatalf("GET leaked a secret: %s", raw)
	}
	if err := migrateNotificationsFile(); err != nil {
		t.Fatal(err)
	}
	stored, _ := os.ReadFile(notificationsFilePath())
	var cfg alert.Config
	json.Unmarshal(stored, &cfg)
	if cfg.HasLegacy() || len(cfg.Channels) != 2 || cfg.Channels[0].Secret != "sig" || cfg.Channels[1].SMTPPass != "p" {
		t.Fatalf("migrated file = %s", stored)
	}
	if fi, _ := os.Stat(notificationsFilePath()); fi.Mode().Perm() != 0600 {
		t.Fatalf("mode %v", fi.Mode().Perm())
	}
}

func TestChannelCRUDNeverReturnsSecrets(t *testing.T) {
	s, _ := alertServer(t)
	code, body := call(t, s.handleNotificationAction, "PUT", "/api/notifications/channels/chat",
		`{"type":"slack","name":"Ops chat","url":"https://hooks.slack.com/services/T0/B0/secretpath"}`)
	if code != 200 {
		t.Fatalf("create: %d %v", code, body)
	}
	v := body["data"].(map[string]any)
	if v["url"] != nil || v["url_hint"] != "https://hooks.slack.com/..." || v["url_set"] != true {
		t.Fatalf("slack URL is a credential and must not round-trip: %v", v)
	}
	code, _ = call(t, s.handleNotificationAction, "PUT", "/api/notifications/channels/chat", `{"type":"discord"}`)
	if code != http.StatusConflict {
		t.Fatalf("type change: %d", code)
	}
	code, _ = call(t, s.handleNotificationAction, "PUT", "/api/notifications/channels/ship", `{"type":"ship","url":"https://ship.example/intake"}`)
	if code != http.StatusBadRequest {
		t.Fatalf("ship without secret: %d", code)
	}
	code, body = call(t, s.handleNotificationAction, "PUT", "/api/notifications/channels/phone",
		`{"type":"ntfy","url":"https://ntfy.example/teploy","token":"tk_secret","kinds":["incident_opened"]}`)
	if code != 200 || body["data"].(map[string]any)["token_set"] != true {
		t.Fatalf("ntfy: %d %v", code, body)
	}
	// A patch without the token keeps it.
	call(t, s.handleNotificationAction, "PUT", "/api/notifications/channels/phone", `{"name":"Phone"}`)
	cfg, _ := loadNotificationsConfig()
	if ch, _ := cfg.Channel("phone"); ch.Token != "tk_secret" || ch.Name != "Phone" {
		t.Fatalf("patch lost token: %+v", ch)
	}
	_, body = call(t, s.handleNotifications, "GET", "/api/notifications", "")
	raw, _ := json.Marshal(body)
	for _, secret := range []string{"tk_secret", "secretpath"} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("GET leaked %q: %s", secret, raw)
		}
	}
	if code, _ := call(t, s.handleNotificationAction, "DELETE", "/api/notifications/channels/chat", ""); code != 200 {
		t.Fatalf("delete %d", code)
	}
	entries := s.alerts.audit.Recent(10)
	if len(entries) < 4 || entries[0].Action != "channel.delete" || entries[0].Actor != "tyler" {
		t.Fatalf("audit = %+v", entries)
	}
}

// Test-send reaches the channel synchronously with kind "test".
func TestChannelTestSend(t *testing.T) {
	s, _ := alertServer(t)
	got := make(chan []byte, 1)
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got <- b
	}))
	defer hook.Close()
	call(t, s.handleNotificationAction, "PUT", "/api/notifications/channels/hook", `{"type":"webhook","url":"`+hook.URL+`"}`)
	code, body := call(t, s.handleNotificationAction, "POST", "/api/notifications/channels/hook/test", "")
	if code != 200 || body["data"].(map[string]any)["ok"] != true {
		t.Fatalf("test-send: %d %v", code, body)
	}
	var w alert.WireEvent
	json.Unmarshal(<-got, &w)
	if w.Kind != alert.KindTest || w.Status != "up" || w.Version != 2 {
		t.Fatalf("test body = %+v", w)
	}
}

// First-enable summary: incidents that opened with no channel are
// announced once when the first channel is saved.
func TestFirstChannelAnnouncesSummary(t *testing.T) {
	s, sink := alertServer(t)
	for _, id := range []string{"a", "b"} {
		r := incident.Rule{Source: alert.SourceMonitor, ID: id, Name: id, Policy: incident.Policy{FailAfter: 1}}
		s.alerts.tracker.Observe(r, false, time.Now(), "down")
	}
	if len(sink.events()) != 0 {
		t.Fatal("opened with no channel configured were sent")
	}
	call(t, s.handleNotificationAction, "PUT", "/api/notifications/channels/phone", `{"type":"ntfy","url":"https://ntfy.example/t"}`)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && len(sink.events()) == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	evs := sink.events()
	if len(evs) != 3 || evs[0].Kind != alert.KindSummary || evs[0].Audience != alert.AudienceHuman {
		t.Fatalf("summary events = %+v", evs)
	}
	// A second channel does not re-announce.
	call(t, s.handleNotificationAction, "PUT", "/api/notifications/channels/hook", `{"type":"webhook","url":"https://h.example"}`)
	time.Sleep(20 * time.Millisecond)
	if len(sink.events()) != 3 {
		t.Fatal("second channel re-announced")
	}
}

// Silences: creating one closes what it covers with suppressed_end, holds
// new opens (recorded), and is audited; deleting it lets the next failure
// open.
func TestSilencesHoldAndClose(t *testing.T) {
	s, sink := alertServer(t)
	writeNotifications(t, `{"channels":[{"id":"hook","type":"webhook","url":"https://h.example"}]}`)
	r := incident.Rule{Source: alert.SourceMonitor, ID: "m1", Name: "shop", App: "shop", Policy: incident.Policy{FailAfter: 1}}
	s.alerts.tracker.Observe(r, false, time.Now(), "down")
	code, body := call(t, s.handleSilences, "POST", "/api/alerts/silences", `{"scope":"app","target":"shop","reason":"migration","duration_minutes":60}`)
	if code != 200 {
		t.Fatalf("create silence: %d %v", code, body)
	}
	evs := sink.events()
	if len(evs) != 2 || evs[1].Reason != alert.ReasonSuppressedEnd {
		t.Fatalf("silence close: %+v", evs)
	}
	s.alerts.tracker.Observe(r, false, time.Now(), "down")
	if len(sink.events()) != 2 || len(sink.sup) != 1 || !strings.HasPrefix(sink.sup[0], "silenced:") {
		t.Fatalf("silenced open: %d sent, suppressed %v", len(sink.events()), sink.sup)
	}
	if code, _ := call(t, s.handleSilences, "POST", "/api/alerts/silences", `{"scope":"app","duration_minutes":5}`); code != 400 {
		t.Fatalf("app silence without target: %d", code)
	}
	if code, _ := call(t, s.handleSilences, "POST", "/api/alerts/silences", `{"scope":"all","duration_minutes":99999}`); code != 400 {
		t.Fatalf("over-long silence: %d", code)
	}
	id := body["data"].(map[string]any)["id"].(string)
	if code, _ := call(t, s.handleSilences, "DELETE", "/api/alerts/silences/"+id, ""); code != 200 {
		t.Fatalf("delete silence: %d", code)
	}
	s.alerts.tracker.Observe(r, false, time.Now(), "down")
	if evs := sink.events(); len(evs) != 3 || evs[2].Kind != alert.KindOpened {
		t.Fatalf("after silence: %+v", evs)
	}
	actions := map[string]bool{}
	for _, e := range s.alerts.audit.Recent(10) {
		actions[e.Action] = true
	}
	if !actions["silence.create"] || !actions["silence.delete"] {
		t.Fatalf("audit = %v", actions)
	}
}

func opAt(kind operation.Kind, status operation.Status, created time.Time, started, finished *time.Time) *operation.Operation {
	return &operation.Operation{ID: "op1", Request: operation.Request{Kind: kind, Server: "s", App: "shop"},
		Status: status, CreatedAt: created, StartedAt: started, FinishedAt: finished}
}

// Suppression windows: a running deploy holds until grace + its 30m
// timeout from start (a hung deploy cannot hide an outage), a finished one
// for the grace only.
func TestOperationHoldWindowsAndCap(t *testing.T) {
	now := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	ago := func(d time.Duration) *time.Time { t := now.Add(-d); return &t }
	cases := []struct {
		name string
		op   *operation.Operation
		want bool
	}{
		{"running 5m", opAt(operation.KindDeploy, operation.StatusRunning, now.Add(-6*time.Minute), ago(5*time.Minute), nil), true},
		{"running 31m (inside grace+timeout)", opAt(operation.KindDeploy, operation.StatusRunning, now.Add(-31*time.Minute), ago(31*time.Minute), nil), true},
		{"hung 33m (past grace+timeout)", opAt(operation.KindDeploy, operation.StatusRunning, now.Add(-33*time.Minute), ago(33*time.Minute), nil), false},
		{"queued 10m", opAt(operation.KindDeploy, operation.StatusQueued, now.Add(-10*time.Minute), nil, nil), true},
		{"queued 40m", opAt(operation.KindDeploy, operation.StatusQueued, now.Add(-40*time.Minute), nil, nil), false},
		{"finished 1m ago", opAt(operation.KindDeploy, operation.StatusSucceeded, now.Add(-10*time.Minute), ago(9*time.Minute), ago(time.Minute)), true},
		{"finished 3m ago", opAt(operation.KindDeploy, operation.StatusFailed, now.Add(-10*time.Minute), ago(9*time.Minute), ago(3*time.Minute)), false},
		{"maintenance op 8m (5m timeout + 2m grace)", opAt(operation.KindMaintenance, operation.StatusRunning, now.Add(-8*time.Minute), ago(8*time.Minute), nil), false},
	}
	for _, c := range cases {
		if got, _ := operationHolds(c.op, now); got != c.want {
			t.Errorf("%s: held=%v, want %v", c.name, got, c.want)
		}
	}
}

// A deploy that causes two failed checks sends nothing: the third check
// lands inside the hold; once the deploy and its grace end, a still-failing
// monitor opens.
func TestDeployHoldsMonitorOpenThroughOperationManager(t *testing.T) {
	withTempGroupsFile(t)
	release := make(chan struct{})
	s := New(Config{DataDir: t.TempDir(), CLIInstalled: func() bool { return false },
		OperationResolver: func(name string) (operation.Server, error) {
			return operation.Server{Name: name, Host: "203.0.113.1", User: "root"}, nil
		},
		OperationExecutor: func(ctx context.Context, c operation.Command, emit func(operation.Stream, string)) (int, error) {
			<-release
			return 0, nil
		}})
	t.Cleanup(func() { s.DrainOperations(context.Background()) })
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(unblock) // runs before the drain, even when the test fails early
	sink := &captureSink{}
	s.alerts.tracker.SetAlerter(sink)
	if _, _, err := s.operations.Enqueue(operation.Request{Kind: operation.KindDeploy, Server: "s", App: "shop"}, "", nil); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	rule := incident.Rule{Source: alert.SourceMonitor, ID: "m1", Name: "shop.example", App: "shop", Server: "s", Policy: incident.Policy{FailAfter: 3, RecoverAfter: 2}}
	for i := 0; i < 4; i++ {
		s.alerts.tracker.Observe(rule, false, time.Now(), "502")
	}
	if len(sink.events()) != 0 || len(sink.sup) != 1 {
		t.Fatalf("deploy hold: sent %d, suppressed %v", len(sink.events()), sink.sup)
	}
	unblock()
	if held, _ := s.alertHold(rule, time.Now().Add(alertHoldGrace+time.Minute)); held {
		// The op may still be finishing; poll briefly.
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			if h, _ := s.alertHold(rule, time.Now().Add(alertHoldGrace+time.Minute)); !h {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	if held, why := s.alertHold(rule, time.Now().Add(alertHoldGrace+time.Minute)); held {
		t.Fatalf("still held after deploy + grace: %s", why)
	}
}

// Maintenance and deploy-lock flags from the fleet cache hold, with grace
// after they clear; the lock hold is capped.
func TestFleetFlagHolds(t *testing.T) {
	s, _ := alertServer(t)
	publish := func(app remote.AppState) {
		gen, _ := s.fleet.beginRefresh()
		s.fleet.publish(gen, []ServerObservation{{Server: "s", Apps: []remote.AppState{app}}})
		s.fleet.endRefresh()
	}
	rule := incident.Rule{App: "shop", Server: "s"}
	now := time.Now()
	publish(remote.AppState{App: "shop", Server: "s", Maintenance: true})
	if held, why := s.alertHold(rule, now); !held || !strings.Contains(why, "maintenance") {
		t.Fatalf("maintenance: %v %s", held, why)
	}
	publish(remote.AppState{App: "shop", Server: "s"})
	if held, _ := s.alertHold(rule, now.Add(time.Minute)); !held {
		t.Fatal("no grace after maintenance ended")
	}
	if held, _ := s.alertHold(rule, now.Add(3*time.Minute)); held {
		t.Fatal("held past grace")
	}
	publish(remote.AppState{App: "shop", Server: "s", Locked: true})
	if held, _ := s.alertHold(rule, now.Add(4*time.Minute)); !held {
		t.Fatal("deploy lock not held")
	}
	if held, _ := s.alertHold(rule, now.Add(4*time.Minute+lockHoldCap+alertHoldGrace+time.Minute)); held {
		t.Fatal("stale lock held past its cap")
	}
}

// Operation failures open an incident with source "operation"; the next
// success of the same kind on the same app recovers it.
func TestOperationFailureIncident(t *testing.T) {
	s, sink := alertServer(t)
	writeNotifications(t, `{"channels":[{"id":"hook","type":"webhook","url":"https://h.example"}]}`)
	fin := time.Now()
	exit := 1
	s.onOperationFinished(&operation.Operation{ID: "op1", Request: operation.Request{Kind: operation.KindDeploy, Server: "s", App: "shop"},
		Status: operation.StatusFailed, ExitCode: &exit, FinishedAt: &fin})
	s.onOperationFinished(&operation.Operation{ID: "op2", Request: operation.Request{Kind: operation.KindRollback, Server: "s", App: "shop"},
		Status: operation.StatusSucceeded, FinishedAt: &fin})
	s.onOperationFinished(&operation.Operation{ID: "op3", Request: operation.Request{Kind: operation.KindDeploy, Server: "s", App: "shop"},
		Status: operation.StatusSucceeded, FinishedAt: &fin})
	s.onOperationFinished(&operation.Operation{ID: "op4", Request: operation.Request{Kind: operation.KindTemplateInstall, Server: "s"},
		Status: operation.StatusFailed, FinishedAt: &fin})
	evs := sink.events()
	if len(evs) != 2 || evs[0].Source != alert.SourceOperation || evs[0].Kind != alert.KindOpened || evs[0].Message != "exit code 1" ||
		evs[0].App != "shop" || evs[0].Server != "s" || evs[1].Kind != alert.KindRecovered || evs[1].IncidentID != evs[0].IncidentID {
		t.Fatalf("operation incident events = %+v", evs)
	}
}

// The push endpoint is reachable without a session, scoped by token, and
// rate-limited.
func TestPushEndpointThroughGate(t *testing.T) {
	withTempGroupsFile(t)
	dir := t.TempDir()
	st := store.NewFileStore(t.TempDir())
	mon := monitor.New(st)
	defer mon.Stop(context.Background())
	s := New(Config{DataDir: dir, Store: st, Monitor: mon, AuthUser: "admin", AuthPass: "correct horse battery", CLIInstalled: func() bool { return false }})
	t.Cleanup(func() { s.DrainOperations(context.Background()) })
	sink := &captureSink{}
	s.alerts.tracker.SetAlerter(sink)
	mon.SetTracker(s.alerts.tracker)
	m := store.Monitor{ID: "p1", Name: "nightly", Type: "push", Interval: time.Hour, Grace: time.Minute, Enabled: true, PushToken: strings.Repeat("cd", 32)}
	if err := st.SaveMonitor(&m); err != nil {
		t.Fatal(err)
	}
	mon.Reload(m)
	h := s.handler()
	do := func(path string) int {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		return w.Code
	}
	if c := do("/api/push/" + m.PushToken + "?msg=ok"); c != 200 {
		t.Fatalf("valid ping: %d", c)
	}
	if c := do("/api/push/" + strings.Repeat("ef", 32)); c != 404 {
		t.Fatalf("unknown token: %d", c)
	}
	if c := do("/api/monitors"); c != 401 {
		t.Fatalf("the push bypass must not open other routes: %d", c)
	}
	limited := false
	for i := 0; i < pushPerTokenPerMinute+2; i++ {
		if do("/api/push/"+m.PushToken) == http.StatusTooManyRequests {
			limited = true
			break
		}
	}
	if !limited {
		t.Fatal("per-token rate limit never applied")
	}
	checks, _ := st.GetChecks("p1", time.Time{}, 1)
	if len(checks) == 0 || checks[0].Status != "up" {
		t.Fatalf("ping not recorded: %+v", checks)
	}
}

// Monitor POST: a push monitor gets a server-minted token that survives
// edits and ignores a client-supplied one.
func TestPushMonitorTokenIsServerAssigned(t *testing.T) {
	s := monitorsTestServer(t)
	body := `{"id":"p1","name":"backup","type":"push","interval":86400000000000,"push_token":"attacker-chosen"}`
	if w := postMonitor(t, s, RoleAdmin, body); w.Code != 200 {
		t.Fatalf("create push: %d %s", w.Code, w.Body.String())
	}
	m, _ := s.store.GetMonitor("p1")
	if len(m.PushToken) != 64 || m.PushToken == "attacker-chosen" || m.Target != "push" || m.Grace != 24*time.Hour/10 {
		t.Fatalf("push monitor = %+v", m)
	}
	first := m.PushToken
	postMonitor(t, s, RoleAdmin, `{"id":"p1","name":"backup renamed","type":"push","interval":86400000000000,"fail_after":2}`)
	m, _ = s.store.GetMonitor("p1")
	if m.PushToken != first || m.FailAfter != 2 {
		t.Fatalf("edit rotated the token or lost policy: %+v", m)
	}
	if w := postMonitor(t, s, RoleAdmin, `{"id":"p2","name":"x","type":"http","target":"https://x.example","fail_after":50}`); w.Code != 400 {
		t.Fatalf("fail_after bound: %d", w.Code)
	}
}

// Status page: open monitor incidents and maintenance are listed by
// monitor name only; restore-test/operation incidents stay internal.
func TestStatusPageIncidentsAndMaintenance(t *testing.T) {
	s := statusTestServer(t, true)
	s.fleet = &fleetCache{ttl: time.Minute}
	s.initAlerting(t.TempDir())
	s.alerts.tracker.Observe(incident.Rule{Source: alert.SourceMonitor, ID: "api", Name: "API", Target: "http://10.0.0.5:8080", Policy: incident.Policy{FailAfter: 1}}, false, time.Now(), "refused")
	s.alerts.tracker.Observe(incident.Rule{Source: alert.SourceOperation, ID: "op-deploy-s-web", Name: "deploy of web on s", Policy: incident.Policy{FailAfter: 1}}, false, time.Now(), "exit 1")
	s.alerts.silences.Add(Silence{ID: "x", Scope: silenceMonitor, Target: "web", Reason: "secret reason", ExpiresAt: time.Now().Add(time.Hour)})
	w := httptest.NewRecorder()
	s.handleStatusAPI(w, httptest.NewRequest("GET", "/api/status", nil))
	raw := w.Body.String()
	var resp statusResponse
	json.Unmarshal(w.Body.Bytes(), &resp)
	if len(resp.Incidents) != 1 || resp.Incidents[0].Name != "API" {
		t.Fatalf("incidents = %+v", resp.Incidents)
	}
	if len(resp.Maintenance) != 1 || resp.Maintenance[0].Name != "Web" || resp.Maintenance[0].Until == nil {
		t.Fatalf("maintenance = %+v", resp.Maintenance)
	}
	for _, leak := range []string{"10.0.0.5", "internal.example.com", "secret reason", "deploy of web", "refused"} {
		if strings.Contains(raw, leak) {
			t.Fatalf("status page leaked %q: %s", leak, raw)
		}
	}
}

// Channel health: a dead-lettered channel alerts through the other channel
// (never through itself) and recovers when it delivers again.
func TestChannelDeadLetterAlertsThroughOtherChannel(t *testing.T) {
	s, sink := alertServer(t)
	call(t, s.handleNotificationAction, "PUT", "/api/notifications/channels/hook", `{"type":"webhook","url":"https://h.example"}`)
	s.OnChannelDeadLetter("hook", alert.Event{}, "HTTP 500")
	if len(sink.events()) != 0 {
		t.Fatal("alerted with no other channel")
	}
	call(t, s.handleNotificationAction, "PUT", "/api/notifications/channels/phone", `{"type":"ntfy","url":"https://ntfy.example/t"}`)
	s.OnChannelDeadLetter("hook", alert.Event{}, "HTTP 500")
	evs := sink.events()
	if len(evs) != 1 || evs[0].Source != alert.SourceAudit || len(evs[0].ExcludeChannels) != 1 || evs[0].ExcludeChannels[0] != "hook" {
		t.Fatalf("channel health = %+v", evs)
	}
	cfg, _ := loadNotificationsConfig()
	if got := alert.ChannelsFor(cfg, evs[0]); len(got) != 1 || got[0] != "phone" {
		t.Fatalf("routed to %v", got)
	}
	s.OnChannelDelivered("hook", alert.Event{})
	if evs := sink.events(); len(evs) != 2 || evs[1].Kind != alert.KindRecovered {
		t.Fatalf("channel recovery = %+v", evs)
	}
}

func TestHeartbeatReceiverCRUD(t *testing.T) {
	s, _ := alertServer(t)
	code, body := call(t, s.handleNotificationAction, "PUT", "/api/notifications/heartbeats/hc", `{"name":"healthchecks.io","url":"http://127.0.0.1:9/ping/uuid"}`)
	if code != 200 {
		t.Fatalf("save: %d %v", code, body)
	}
	v := body["data"].(map[string]any)
	if !strings.Contains(v["warning"].(string), "this host") || strings.Contains(v["url_hint"].(string), "uuid") {
		t.Fatalf("view = %v", v)
	}
	code, body = call(t, s.handleNotificationAction, "POST", "/api/notifications/heartbeats/hc/test", "")
	if code != 200 || body["data"].(map[string]any)["ok"] != false {
		t.Fatalf("test ping to a closed port: %d %v", code, body)
	}
	if code, _ := call(t, s.handleNotificationAction, "PUT", "/api/notifications/heartbeats/bad", `{"url":"ftp://x"}`); code != 400 {
		t.Fatalf("bad URL: %d", code)
	}
	if code, _ := call(t, s.handleNotificationAction, "DELETE", "/api/notifications/heartbeats/hc", ""); code != 200 {
		t.Fatalf("delete: %d", code)
	}
}

// The push token reaches only principals who can edit monitors; the
// push-url endpoint serves it to them (and is audited).
func TestPushTokenHiddenFromViewers(t *testing.T) {
	s, _ := alertServer(t)
	m := store.Monitor{ID: "p1", Name: "backup", Type: "push", Interval: time.Hour, Enabled: true, PushToken: strings.Repeat("ab", 32)}
	if err := s.store.SaveMonitor(&m); err != nil {
		t.Fatal(err)
	}
	get := func(role, path string, h http.HandlerFunc) string {
		req := withUser(httptest.NewRequest("GET", path, nil), &sessionInfo{user: role, role: role, caps: caps.PresetForRole(role)})
		w := httptest.NewRecorder()
		h(w, req)
		return w.Body.String()
	}
	for _, path := range []string{"/api/monitors", "/api/monitors/p1"} {
		h := s.handleMonitors
		if path != "/api/monitors" {
			h = s.handleMonitor
		}
		if body := get(RoleViewer, path, h); strings.Contains(body, m.PushToken) {
			t.Fatalf("viewer saw the push token at %s", path)
		}
		if body := get(RoleEditor, path, h); !strings.Contains(body, m.PushToken) {
			t.Fatalf("editor lost the push token at %s", path)
		}
	}
	if body := get(RoleEditor, "/api/monitors/p1/push-url", s.handleMonitor); !strings.Contains(body, "/api/push/"+m.PushToken) {
		t.Fatalf("push-url = %s", body)
	}
	if e := s.alerts.audit.Recent(1); len(e) != 1 || e[0].Action != "monitor.push_url" {
		t.Fatalf("audit = %+v", e)
	}
	if got := requiredCapabilities("GET", "/api/monitors/p1/push-url"); len(got) != 1 || got[0] != caps.ExecuteMutate {
		t.Fatalf("push-url caps = %v", got)
	}
}

// Valid pings are not counted against the address (one host can run many
// jobs); unknown-token misses are, and short paths store nothing.
func TestPushLimitCountsOnlyMisses(t *testing.T) {
	withTempGroupsFile(t)
	st := store.NewFileStore(t.TempDir())
	mon := monitor.New(st)
	defer mon.Stop(context.Background())
	s := New(Config{DataDir: t.TempDir(), Store: st, Monitor: mon, NoAuth: true, CLIInstalled: func() bool { return false }})
	t.Cleanup(func() { s.DrainOperations(context.Background()) })
	mon.SetTracker(s.alerts.tracker)
	var tokens []string
	for i := 0; i < 8; i++ {
		tok := strings.Repeat(string(rune('a'+i)), 64)
		m := store.Monitor{ID: "p" + string(rune('a'+i)), Name: "job", Type: "push", Interval: time.Hour, Grace: time.Minute, Enabled: true, PushToken: tok}
		st.SaveMonitor(&m)
		mon.Reload(m)
		tokens = append(tokens, tok)
	}
	do := func(path string) int {
		w := httptest.NewRecorder()
		s.handlePush(w, httptest.NewRequest("GET", path, nil))
		return w.Code
	}
	for round := 0; round < 13; round++ { // 104 valid pings from one address
		for _, tok := range tokens {
			if c := do("/api/push/" + tok); c != 200 && c != http.StatusTooManyRequests {
				t.Fatalf("valid ping: %d", c)
			}
		}
	}
	if c := do("/api/push/" + tokens[0]); c != http.StatusTooManyRequests {
		t.Fatalf("per-token limit: %d", c)
	}
	if c := do("/api/push/x"); c != 404 {
		t.Fatalf("short token: %d", c)
	}
	s.alerts.pushLimit.mu.Lock()
	keys := len(s.alerts.pushLimit.windows)
	s.alerts.pushLimit.mu.Unlock()
	if keys != len(tokens) {
		t.Fatalf("limiter keys = %d, want only the %d known tokens", keys, len(tokens))
	}
	for i := 0; i < pushMissesPerIPPerMinute; i++ {
		do("/api/push/" + strings.Repeat("0", 64))
	}
	if c := do("/api/push/" + strings.Repeat("1", 64)); c != http.StatusTooManyRequests {
		t.Fatalf("miss limit: %d", c)
	}
}

// Deleting a channel retires its health incident; at startup incidents of
// rules that no longer exist are retired.
func TestRetireDeletedChannelAndOrphans(t *testing.T) {
	s, sink := alertServer(t)
	writeNotifications(t, `{"channels":[{"id":"hook","type":"webhook","url":"https://h.example"},{"id":"phone","type":"ntfy","url":"https://n.example/t"}]}`)
	s.OnChannelDeadLetter("hook", alert.Event{}, "HTTP 500")
	call(t, s.handleNotificationAction, "DELETE", "/api/notifications/channels/hook", "")
	evs := sink.events()
	if len(evs) != 2 || evs[1].Reason != alert.ReasonRetired {
		t.Fatalf("channel delete: %+v", evs)
	}
	m := store.Monitor{ID: "gone", Name: "gone", Type: "http", Target: "https://g.example", Enabled: false}
	s.store.SaveMonitor(&m)
	s.alerts.tracker.Observe(incident.Rule{Key: "monitor:gone", Source: alert.SourceMonitor, ID: "gone", Name: "gone", Policy: incident.Policy{FailAfter: 1}}, false, time.Now(), "x")
	s.alerts.tracker.Observe(incident.Rule{Key: "monitor:nope", Source: alert.SourceMonitor, ID: "nope", Name: "nope", Policy: incident.Policy{FailAfter: 1}}, false, time.Now(), "x")
	s.alerts.tracker.Observe(incident.Rule{Key: "restore-test:rt", Source: alert.SourceRestoreTest, ID: "rt", Name: "rt", Policy: incident.Policy{FailAfter: 1}}, false, time.Now(), "x")
	s.retireOrphanIncidents()
	if open := s.alerts.tracker.Open(); len(open) != 0 {
		t.Fatalf("orphans left open: %+v", open)
	}
}

// Delivery errors served to metadata readers never carry a credential URL.
func TestDeliveryErrorsRedacted(t *testing.T) {
	withTempGroupsFile(t)
	writeNotifications(t, `{"channels":[{"id":"chat","type":"slack","url":"https://hooks.slack.com/services/T/B/SECRETPATH"}]}`)
	redactMu.Lock()
	redactAt = time.Time{}
	redactMu.Unlock()
	if got := RedactAlertText(`Post "https://hooks.slack.com/services/T/B/SECRETPATH": dial tcp`); strings.Contains(got, "SECRETPATH") {
		t.Fatalf("redacted = %q", got)
	}
}

// Maintenance holds, but not forever.
func TestMaintenanceHoldCapped(t *testing.T) {
	s, _ := alertServer(t)
	gen, _ := s.fleet.beginRefresh()
	s.fleet.publish(gen, []ServerObservation{{Server: "s", Apps: []remote.AppState{{App: "shop", Server: "s", Maintenance: true}}}})
	s.fleet.endRefresh()
	rule := incident.Rule{App: "shop", Server: "s"}
	now := time.Now()
	if held, _ := s.alertHold(rule, now); !held {
		t.Fatal("maintenance not held")
	}
	if held, _ := s.alertHold(rule, now.Add(maintenanceHoldCap+alertHoldGrace+time.Minute)); held {
		t.Fatal("maintenance held past its cap")
	}
}
