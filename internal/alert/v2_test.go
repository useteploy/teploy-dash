package alert

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type captured struct {
	header http.Header
	body   []byte
}

func captureServer(t *testing.T, status int, respond func(w http.ResponseWriter)) (*httptest.Server, chan captured) {
	t.Helper()
	ch := make(chan captured, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		ch <- captured{header: r.Header.Clone(), body: b}
		if respond != nil {
			respond(w)
		}
		w.WriteHeader(status)
	}))
	t.Cleanup(srv.Close)
	return srv, ch
}

func v2Event() Event {
	started := time.Date(2026, 11, 3, 22, 28, 0, 0, time.UTC)
	return Event{
		MonitorID: "fleet-8f1c", MonitorName: "bugsandwishes.com", Status: "down",
		Message: "unexpected status 502", OccurredAt: started.Add(3 * time.Minute),
		Source: SourceMonitor, DeliveryID: "9c2e", Version: 2, Kind: KindOpened,
		IncidentID: "inc_3f9a", RuleID: "fleet-8f1c", Service: "akiroo-lite", App: "akiroo-lite",
		Server: "deploy-home2", Target: "https://bugsandwishes.com/", Severity: SeverityCritical,
		StartedAt: started, ConsecutiveFailures: 3, URL: "https://dash.example.internal/monitors/fleet-8f1c",
	}
}

// The webhook body is EXACTLY the plan 2.5 contract: every field present, in
// contract order, internal routing fields absent.
func TestWireBodyMatchesContract(t *testing.T) {
	ev := v2Event()
	ev.ExcludeChannels = []string{"x"}
	ev.Audience = AudienceMachine
	raw, err := json.Marshal(ev.Wire())
	if err != nil {
		t.Fatal(err)
	}
	wantKeys := []string{"monitor_id", "monitor_name", "status", "message", "occurred_at", "source", "delivery_id",
		"version", "kind", "reason", "incident_id", "rule_id", "service", "app", "server", "target", "severity",
		"started_at", "duration_s", "consecutive_failures", "url"}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.Token()
	var got []string
	for dec.More() {
		k, _ := dec.Token()
		got = append(got, k.(string))
		var skip json.RawMessage
		dec.Decode(&skip)
	}
	if strings.Join(got, ",") != strings.Join(wantKeys, ",") {
		t.Fatalf("wire keys:\n got %v\nwant %v", got, wantKeys)
	}
	var m map[string]any
	json.Unmarshal(raw, &m)
	if m["version"].(float64) != 2 || m["kind"] != KindOpened || m["status"] != "down" ||
		m["started_at"] != "2026-11-03T22:28:00Z" || m["occurred_at"] != "2026-11-03T22:31:00Z" ||
		m["duration_s"].(float64) != 0 || m["consecutive_failures"].(float64) != 3 {
		t.Fatalf("wire values wrong: %s", raw)
	}
}

// Every kind keeps status "down"/"up" (Akiroo rejects anything else).
func TestWireStatusAlwaysDownOrUp(t *testing.T) {
	for _, tc := range []struct{ kind, status, want string }{
		{KindOpened, "down", "down"}, {KindRepeat, "down", "down"}, {KindRecovered, "up", "up"},
		{KindTest, "up", "up"}, {KindOpened, "timeout", "down"},
	} {
		ev := v2Event()
		ev.Kind, ev.Status = tc.kind, tc.status
		if got := ev.Wire().Status; got != tc.want {
			t.Errorf("kind %s status %s -> %q, want %q", tc.kind, tc.status, got, tc.want)
		}
	}
}

// A pre-v2 outbox record (journaled before the upgrade) still produces a
// complete v2 body.
func TestWireDerivesV2FromLegacyRecord(t *testing.T) {
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	w := Event{MonitorID: "m1", MonitorName: "api", Status: "up", OccurredAt: at}.Wire()
	if w.Version != 2 || w.Kind != KindRecovered || w.RuleID != "m1" || w.Service != "api" ||
		w.Source != SourceMonitor || w.StartedAt != "2026-01-01T00:00:00Z" {
		t.Fatalf("legacy wire = %+v", w)
	}
	if (Event{MonitorID: "m1", Status: "down"}).Wire().Kind != KindOpened {
		t.Fatal("legacy down must read as incident_opened")
	}
}

func TestWebhookHeadersAndSignature(t *testing.T) {
	srv, got := captureServer(t, 204, nil)
	cfg := Config{Channels: []Channel{{ID: "hook", Type: TypeWebhook, URL: srv.URL, Secret: "s3cret"}}}
	if err := New(cfg).SendChannel(v2Event(), "hook"); err != nil {
		t.Fatal(err)
	}
	c := <-got
	if c.header.Get("X-Teploy-Delivery") != "9c2e" || c.header.Get("X-Teploy-Event-Version") != "2" {
		t.Fatalf("headers = %v", c.header)
	}
	ts := c.header.Get("X-Teploy-Timestamp")
	mac := hmac.New(sha256.New, []byte("s3cret"))
	mac.Write([]byte(ts + "."))
	mac.Write(c.body)
	if c.header.Get("X-Teploy-Signature") != "sha256="+hex.EncodeToString(mac.Sum(nil)) {
		t.Fatal("signature does not verify")
	}
	var w WireEvent
	if json.Unmarshal(c.body, &w) != nil || w.IncidentID != "inc_3f9a" {
		t.Fatalf("body = %s", c.body)
	}
}

// Ship is a signed v2 webhook with its own secret; it is unusable without one.
func TestShipChannelSignedAndRequiresSecret(t *testing.T) {
	srv, got := captureServer(t, 200, nil)
	if (Channel{ID: "ship", Type: TypeShip, URL: srv.URL}).Usable() {
		t.Fatal("ship without a secret must not count as configured")
	}
	if ValidateChannel(Channel{ID: "ship", Type: TypeShip, URL: srv.URL}) == nil {
		t.Fatal("ship without a secret must be refused")
	}
	cfg := Config{Channels: []Channel{{ID: "ship", Type: TypeShip, URL: srv.URL, Secret: "intake"}}}
	if err := New(cfg).SendChannel(v2Event(), "ship"); err != nil {
		t.Fatal(err)
	}
	c := <-got
	if c.header.Get("X-Teploy-Signature") == "" || c.header.Get("X-Teploy-Event-Version") != "2" {
		t.Fatalf("ship headers = %v", c.header)
	}
}

func TestSlackCompatiblePayload(t *testing.T) {
	srv, got := captureServer(t, 200, nil)
	cfg := Config{Channels: []Channel{{ID: "chat", Type: TypeSlack, URL: srv.URL}}}
	if err := New(cfg).SendChannel(v2Event(), "chat"); err != nil {
		t.Fatal(err)
	}
	var body map[string]string
	if err := json.Unmarshal((<-got).body, &body); err != nil {
		t.Fatal(err)
	}
	if len(body) != 1 || !strings.HasPrefix(body["text"], "Down: bugsandwishes.com\n") ||
		!strings.Contains(body["text"], "unexpected status 502") {
		t.Fatalf("slack body = %v", body)
	}
}

func TestDiscordEmbedTruncatedAndRetryAfter(t *testing.T) {
	srv, got := captureServer(t, 200, nil)
	cfg := Config{Channels: []Channel{{ID: "dc", Type: TypeDiscord, URL: srv.URL}}}
	ev := v2Event()
	ev.Message = strings.Repeat("x", 5000)
	if err := New(cfg).SendChannel(ev, "dc"); err != nil {
		t.Fatal(err)
	}
	var body struct {
		Embeds []struct {
			Title       string `json:"title"`
			Description string `json:"description"`
		} `json:"embeds"`
	}
	json.Unmarshal((<-got).body, &body)
	if len(body.Embeds) != 1 || len([]rune(body.Embeds[0].Description)) > 2000 || body.Embeds[0].Title == "" {
		t.Fatalf("discord embed = %+v", body)
	}

	limited, _ := captureServer(t, http.StatusTooManyRequests, func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write([]byte(`{"message":"You are being rate limited.","retry_after":7.5,"global":false}`))
	})
	cfg.Channels[0].URL = limited.URL
	err := New(cfg).SendChannel(v2Event(), "dc")
	if err == nil || RetryAfter(err) != 7500*time.Millisecond {
		t.Fatalf("discord 429 err = %v (retry %s), want retry_after 7.5s", err, RetryAfter(err))
	}
}

func TestRetryAfterHeaderHonored(t *testing.T) {
	for _, tc := range []struct {
		status int
		value  string
		want   time.Duration
	}{
		{429, "30", 30 * time.Second},
		{503, "120", 2 * time.Minute},
		{429, "999999", maxRetryAfter},
		{500, "30", 0},
	} {
		srv, _ := captureServer(t, tc.status, func(w http.ResponseWriter) { w.Header().Set("Retry-After", tc.value) })
		cfg := Config{Channels: []Channel{{ID: "h", Type: TypeWebhook, URL: srv.URL}}}
		err := New(cfg).SendChannel(v2Event(), "h")
		if err == nil {
			t.Fatalf("%d must fail", tc.status)
		}
		if got := RetryAfter(err); got != tc.want {
			t.Errorf("status %d Retry-After %s -> %s, want %s", tc.status, tc.value, got, tc.want)
		}
	}
	if d := parseRetryAfter(time.Now().Add(90*time.Second).UTC().Format(http.TimeFormat), time.Now()); d < 80*time.Second || d > 91*time.Second {
		t.Fatalf("HTTP-date Retry-After = %s", d)
	}
}

func TestNtfyHeaders(t *testing.T) {
	srv, got := captureServer(t, 200, nil)
	cfg := Config{Channels: []Channel{{ID: "phone", Type: TypeNtfy, URL: srv.URL + "/teploy-alerts", Token: "tk_abc"}}}
	if err := New(cfg).SendChannel(v2Event(), "phone"); err != nil {
		t.Fatal(err)
	}
	c := <-got
	h := c.header
	if h.Get("Title") != "Down: bugsandwishes.com" || h.Get("Priority") != "urgent" ||
		h.Get("Click") != "https://dash.example.internal/monitors/fleet-8f1c" ||
		h.Get("Authorization") != "Bearer tk_abc" {
		t.Fatalf("ntfy headers = %v", h)
	}
	// Plain words only: ntfy turns emoji shortcode names (warning, rotating_light,
	// white_check_mark, ...) into emoji; none may appear.
	for _, tag := range strings.Split(h.Get("Tags"), ",") {
		switch tag {
		case "teploy", "down", "recovered", "test", "monitor", "restore-test", "operation", "heartbeat", "audit", "backup", "domain":
		default:
			t.Fatalf("unexpected ntfy tag %q", tag)
		}
	}
	if !strings.Contains(string(c.body), "unexpected status 502") {
		t.Fatalf("ntfy body = %q", c.body)
	}

	rec := v2Event()
	rec.Kind, rec.Status = KindRecovered, "up"
	New(cfg).SendChannel(rec, "phone")
	if p := (<-got).header.Get("Priority"); p != "default" {
		t.Fatalf("recovered priority = %q, want default", p)
	}
	warn := v2Event()
	warn.Severity = SeverityWarning
	New(cfg).SendChannel(warn, "phone")
	if p := (<-got).header.Get("Priority"); p != "high" {
		t.Fatalf("warning priority = %q, want high", p)
	}
}

func TestMigrateLegacyConfig(t *testing.T) {
	legacy := Config{WebhookURL: "https://hook.example/x", WebhookSecret: "sig",
		SMTPHost: "smtp.example", SMTPPort: 587, SMTPUser: "u", SMTPPass: "p", EmailTo: "ops@example", EmailFrom: "dash@example"}
	m := legacy.Migrate()
	if m.HasLegacy() || len(m.Channels) != 2 {
		t.Fatalf("migrated = %+v", m)
	}
	if w := m.Channels[0]; w.ID != ChannelWebhook || w.Type != TypeWebhook || w.URL != legacy.WebhookURL || w.Secret != "sig" {
		t.Fatalf("webhook channel = %+v", w)
	}
	if e := m.Channels[1]; e.ID != ChannelEmail || e.Type != TypeEmail || e.SMTPPass != "p" || e.SMTPPort != 587 || e.EmailTo != "ops@example" {
		t.Fatalf("email channel = %+v", e)
	}
	if got := ConfiguredChannels(legacy); strings.Join(got, ",") != "webhook,email" {
		t.Fatalf("configured = %v", got)
	}
	// An explicit channel that already owns the id wins over legacy fields.
	mixed := Config{WebhookURL: "https://old.example", Channels: []Channel{{ID: ChannelWebhook, Type: TypeWebhook, URL: "https://new.example"}}}
	if c, _ := mixed.Channel(ChannelWebhook); c.URL != "https://new.example" {
		t.Fatalf("explicit channel lost to legacy: %+v", c)
	}
}

func TestChannelsForRouting(t *testing.T) {
	cfg := Config{Channels: []Channel{
		{ID: "hook", Type: TypeWebhook, URL: "https://h.example"},
		{ID: "phone", Type: TypeNtfy, URL: "https://ntfy.example/t", Kinds: []string{KindOpened}},
		{ID: "half", Type: TypeSlack},
	}}
	ev := v2Event()
	if got := strings.Join(ChannelsFor(cfg, ev), ","); got != "hook,phone" {
		t.Fatalf("opened -> %s", got)
	}
	ev.Kind = KindRecovered
	if got := strings.Join(ChannelsFor(cfg, ev), ","); got != "hook" {
		t.Fatalf("kinds filter: recovered -> %s", got)
	}
	ev.Kind = KindTest
	if got := strings.Join(ChannelsFor(cfg, ev), ","); got != "hook,phone" {
		t.Fatalf("test ignores kinds -> %s", got)
	}
	ev = v2Event()
	ev.ExcludeChannels = []string{"hook"}
	if got := strings.Join(ChannelsFor(cfg, ev), ","); got != "phone" {
		t.Fatalf("exclude -> %s", got)
	}
	ev = v2Event()
	ev.Kind, ev.Audience = KindSummary, AudienceHuman
	if got := strings.Join(ChannelsFor(cfg, ev), ","); got != "phone" {
		t.Fatalf("summary is human-only -> %s", got)
	}
	ev = v2Event()
	ev.Audience = AudienceMachine
	if got := strings.Join(ChannelsFor(cfg, ev), ","); got != "hook" {
		t.Fatalf("machine audience -> %s", got)
	}
}

func TestHumanTextHasNoEmoji(t *testing.T) {
	for _, kind := range []string{KindOpened, KindRepeat, KindRecovered, KindTest, KindSummary} {
		ev := v2Event()
		ev.Kind = kind
		for _, r := range Title(ev) + Body(ev) {
			if r > 0x2000 {
				t.Fatalf("kind %s renders non-plain rune %U", kind, r)
			}
		}
	}
}

// Transport errors never carry the destination URL (it is the credential).
func TestDeliveryErrorRedactsURL(t *testing.T) {
	cfg := Config{Channels: []Channel{{ID: "chat", Type: TypeSlack, URL: "http://127.0.0.1:9/services/T0/B0/SECRETPATH"}}}
	err := New(cfg).SendChannel(v2Event(), "chat")
	if err == nil || strings.Contains(err.Error(), "SECRETPATH") || !strings.Contains(err.Error(), "http://127.0.0.1:9/...") {
		t.Fatalf("err = %v", err)
	}
	if got := Redact("x https://ntfy.sh/topic-xyz y tok_secret", Config{Channels: []Channel{{ID: "p", Type: TypeNtfy, URL: "https://ntfy.sh/topic-xyz", Token: "tok_secret"}}}); strings.Contains(got, "topic-xyz") || strings.Contains(got, "tok_secret") {
		t.Fatalf("Redact = %q", got)
	}
}

// Slack mrkdwn control characters are escaped (no <!channel> injection).
func TestSlackEscapesControlCharacters(t *testing.T) {
	srv, got := captureServer(t, 200, nil)
	ev := v2Event()
	ev.MonitorName = "<!channel> & co"
	New(Config{Channels: []Channel{{ID: "c", Type: TypeSlack, URL: srv.URL}}}).SendChannel(ev, "c")
	var body map[string]string
	json.Unmarshal((<-got).body, &body)
	if strings.Contains(body["text"], "<!channel>") || !strings.Contains(body["text"], "&lt;!channel&gt; &amp; co") {
		t.Fatalf("text = %q", body["text"])
	}
}

func TestNtfyBodyTruncated(t *testing.T) {
	srv, got := captureServer(t, 200, nil)
	ev := v2Event()
	ev.Message = strings.Repeat("é", 5000)
	New(Config{Channels: []Channel{{ID: "p", Type: TypeNtfy, URL: srv.URL + "/t"}}}).SendChannel(ev, "p")
	if b := (<-got).body; len(b) > 4000 || !utf8Valid(b) {
		t.Fatalf("ntfy body %d bytes", len(b))
	}
}

func utf8Valid(b []byte) bool { return strings.ToValidUTF8(string(b), "?") == string(b) }

// The installed network guard applies to channels unless they opt in.
func TestDialPolicyGuardsChannels(t *testing.T) {
	srv, _ := captureServer(t, 200, nil)
	SetDialPolicy(func(allow bool) func(ctx context.Context, network, addr string) (net.Conn, error) {
		return func(ctx context.Context, network, addr string) (net.Conn, error) {
			if !allow {
				return nil, errors.New("blocked internal address")
			}
			var d net.Dialer
			return d.DialContext(ctx, network, addr)
		}
	})
	defer SetDialPolicy(nil)
	cfg := Config{Channels: []Channel{{ID: "h", Type: TypeWebhook, URL: srv.URL}, {ID: "in", Type: TypeWebhook, URL: srv.URL, AllowInternal: true}}}
	if err := New(cfg).SendChannel(v2Event(), "h"); err == nil || !strings.Contains(err.Error(), "blocked") {
		t.Fatalf("guarded channel: %v", err)
	}
	if err := New(cfg).SendChannel(v2Event(), "in"); err != nil {
		t.Fatalf("allow_internal channel: %v", err)
	}
}
