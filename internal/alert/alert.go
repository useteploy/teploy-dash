package alert

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/smtp"
	"strconv"
	"strings"
	"time"
)

// webhookClient bounds webhook delivery. sendWebhook runs in its own goroutine
// per alert; without a timeout a hanging endpoint would leak a goroutine on
// every state transition.
//
// Redirects are refused outright: Go's default redirect handling converts a
// 301/302/303 POST into a bodyless GET, so a redirect to a friendly landing
// page would consume the attempt (final 2xx) without ever delivering the
// signed JSON — the alert is silently lost. Stopping at the first response
// lets the >= 300 check below classify every redirect as non-delivery.
var webhookClient = &http.Client{
	Timeout: 10 * time.Second,
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		return http.ErrUseLastResponse
	},
}

// Event sources: the label on an Event naming what produced it (plan 2.5).
// Monitors, restore tests, operations and the rest share one alert outbox;
// the label scopes delivery-status lookups and lets receivers route. Empty is
// legacy monitor events (pre-label journals decode as monitors).
const (
	SourceMonitor     = "monitor"
	SourceRestoreTest = "restore-test"
	SourceBackup      = "backup"
	SourceDomain      = "domain"
	SourceOperation   = "operation"
	SourceAudit       = "audit"
	// SourceHeartbeat labels push/heartbeat monitors (a server backup job or
	// an app cron job that pings Dash, plan P3.8).
	SourceHeartbeat = "heartbeat"
)

// Severities (plan 2.5).
const (
	SeverityCritical = "critical"
	SeverityWarning  = "warning"
	SeverityInfo     = "info"
)

// Event is one alert. It is the outbox's persisted record of what to say;
// Wire() projects it onto the normative Event v2 body (plan 2.5). Fields
// below the v1 block are v2 additions; the routing fields at the end are
// internal and never reach a receiver.
type Event struct {
	MonitorID   string    `json:"monitor_id"`
	MonitorName string    `json:"monitor_name"`
	Status      string    `json:"status"` // ALWAYS "down" or "up" (up = recovered)
	Message     string    `json:"message"`
	OccurredAt  time.Time `json:"occurred_at"`
	// Source labels what produced the event (see the Source constants).
	// Empty is legacy monitor events.
	Source string `json:"source,omitempty"`
	// DeliveryID is the outbox's idempotency key for this delivery (D08).
	// Empty on direct (non-outbox) sends; when set it travels in the body,
	// the X-Teploy-Delivery header and an email header so receivers dedupe
	// the at-least-once redelivery the outbox permits.
	DeliveryID string `json:"delivery_id,omitempty"`

	// Event v2 (plan 2.5). Zero on records written before v2; Wire()
	// derives the v2 view of those from the v1 fields.
	Version             int       `json:"version,omitempty"`
	Kind                string    `json:"kind,omitempty"`
	Reason              string    `json:"reason,omitempty"`
	IncidentID          string    `json:"incident_id,omitempty"`
	RuleID              string    `json:"rule_id,omitempty"`
	Service             string    `json:"service,omitempty"`
	App                 string    `json:"app,omitempty"`
	Server              string    `json:"server,omitempty"`
	Target              string    `json:"target,omitempty"`
	Severity            string    `json:"severity,omitempty"`
	StartedAt           time.Time `json:"started_at,omitempty"`
	DurationS           int64     `json:"duration_s,omitempty"`
	ConsecutiveFailures int       `json:"consecutive_failures,omitempty"`
	URL                 string    `json:"url,omitempty"`

	// Internal routing, persisted with the outbox record (so a retry after a
	// restart routes identically) but never serialized onto the wire.
	OnlyChannels    []string `json:"only_channels,omitempty"`
	ExcludeChannels []string `json:"exclude_channels,omitempty"`
	Audience        string   `json:"audience,omitempty"`
}

// WireEvent is the normative Event v2 body (plan 2.5): every field always
// present, in contract order.
type WireEvent struct {
	MonitorID           string `json:"monitor_id"`
	MonitorName         string `json:"monitor_name"`
	Status              string `json:"status"`
	Message             string `json:"message"`
	OccurredAt          string `json:"occurred_at"`
	Source              string `json:"source"`
	DeliveryID          string `json:"delivery_id"`
	Version             int    `json:"version"`
	Kind                string `json:"kind"`
	Reason              string `json:"reason"`
	IncidentID          string `json:"incident_id"`
	RuleID              string `json:"rule_id"`
	Service             string `json:"service"`
	App                 string `json:"app"`
	Server              string `json:"server"`
	Target              string `json:"target"`
	Severity            string `json:"severity"`
	StartedAt           string `json:"started_at"`
	DurationS           int64  `json:"duration_s"`
	ConsecutiveFailures int    `json:"consecutive_failures"`
	URL                 string `json:"url"`
}

// wireKind is the event's kind, derived from status for pre-v2 records.
func (e Event) wireKind() string {
	if e.Kind != "" {
		return e.Kind
	}
	if e.Status == "up" {
		return KindRecovered
	}
	return KindOpened
}

func rfc3339(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// Wire projects the event onto the Event v2 body. A pre-v2 record (an outbox
// entry journaled before the upgrade) gets the derived v2 view: kind from
// status, rule_id = monitor_id, service = monitor name, started_at =
// occurred_at.
func (e Event) Wire() WireEvent {
	status := e.Status
	if status != "up" {
		status = "down"
	}
	source := e.Source
	if source == "" {
		source = SourceMonitor
	}
	ruleID := e.RuleID
	if ruleID == "" {
		ruleID = e.MonitorID
	}
	service := e.Service
	if service == "" {
		service = e.App
	}
	if service == "" {
		service = e.MonitorName
	}
	severity := e.Severity
	if severity == "" {
		severity = SeverityCritical
		if e.wireKind() == KindTest {
			severity = SeverityInfo
		}
	}
	started := e.StartedAt
	if started.IsZero() {
		started = e.OccurredAt
	}
	return WireEvent{
		MonitorID: e.MonitorID, MonitorName: e.MonitorName, Status: status, Message: e.Message,
		OccurredAt: rfc3339(e.OccurredAt), Source: source, DeliveryID: e.DeliveryID,
		Version: 2, Kind: e.wireKind(), Reason: e.Reason, IncidentID: e.IncidentID, RuleID: ruleID,
		Service: service, App: e.App, Server: e.Server, Target: e.Target, Severity: severity,
		StartedAt: rfc3339(started), DurationS: e.DurationS, ConsecutiveFailures: e.ConsecutiveFailures,
		URL: e.URL,
	}
}

// Dispatcher sends alerts via configured channels.
type Dispatcher struct {
	config Config
}

// New creates an alert dispatcher.
func New(config Config) *Dispatcher {
	return &Dispatcher{config: config}
}

// Send dispatches an alert event to all configured channels, best-effort:
// failures are logged, not retried, not surfaced anywhere (fire-and-forget
// callers only — every runner goes through the durable outbox instead).
func (d *Dispatcher) Send(event Event) {
	if len(ChannelsFor(d.config, event)) > 0 {
		go func() {
			if err := d.SendSync(event); err != nil {
				log.Printf("[alert] delivery failed: %v", err)
			}
		}()
	}
}

// SendSync delivers to every channel the event routes to, synchronously, and
// reports the combined failure. A nil return means every channel
// acknowledged the delivery.
func (d *Dispatcher) SendSync(event Event) error {
	var errs []error
	for _, channel := range ChannelsFor(d.config, event) {
		if err := d.SendChannel(event, channel); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", channel, err))
		}
	}
	return errors.Join(errs...)
}

// SendChannel delivers through exactly one channel id (D08 per-channel
// accounting: the outbox retries only the channels that failed). An unknown
// or unusable channel is an error, never a silent skip.
func (d *Dispatcher) SendChannel(event Event, channel string) error {
	ch, ok := d.config.Channel(channel)
	if !ok {
		return fmt.Errorf("unknown alert channel %q", channel)
	}
	if !ch.Usable() {
		return fmt.Errorf("%s channel is not configured", channel)
	}
	link := event.URL
	switch ch.Type {
	case TypeWebhook, TypeShip:
		return sendWebhook(ch, event)
	case TypeSlack:
		return sendSlack(ch, event)
	case TypeDiscord:
		return sendDiscord(ch, event)
	case TypeNtfy:
		return sendNtfy(ch, event, link)
	case TypeEmail:
		return sendEmail(ch, event)
	default:
		return fmt.Errorf("channel %q has unknown type %q", channel, ch.Type)
	}
}

// Webhook deliveries carry the same signature every teploy product sends:
//
//	X-Teploy-Timestamp: <unix seconds>
//	X-Teploy-Signature: sha256=hex(HMAC-SHA256(secret, timestamp + "." + body))
//
// teploy-cli (internal/notify/sign.go) and teploy-observe
// (internal/platform/webhooks.go) sign identically, so a receiver of all three
// writes one verifier. The construction is duplicated rather than shared
// because these are separate binaries in separate modules; if it ever changes,
// it changes in all three or receivers break.
func signWebhook(req *http.Request, secret string, body []byte) {
	if secret == "" {
		return
	}
	ts := strconv.FormatInt(time.Now().UTC().Unix(), 10)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(ts))
	mac.Write([]byte("."))
	mac.Write(body)
	req.Header.Set("X-Teploy-Timestamp", ts)
	req.Header.Set("X-Teploy-Signature", "sha256="+hex.EncodeToString(mac.Sum(nil)))
}

func sendEmail(ch Channel, event Event) error {
	msg := buildEmailMessage(ch.EmailFrom, ch.EmailTo, event)

	addr := net.JoinHostPort(ch.SMTPHost, strconv.Itoa(ch.SMTPPort))
	var auth smtp.Auth
	if ch.SMTPUser != "" {
		auth = smtp.PlainAuth("", ch.SMTPUser, ch.SMTPPass, ch.SMTPHost)
	}

	return sendMailTimeout(addr, ch.SMTPHost, auth, ch.EmailFrom, []string{ch.EmailTo}, []byte(msg), 10*time.Second, ch.SMTPAllowInsecure)
}

// buildEmailMessage constructs the raw RFC 5322 message for an alert email.
// from and to are sanitized (CR/LF stripped) before being placed in headers so
// a configured email_from/email_to can't inject additional SMTP headers
// (e.g. an unwanted Bcc). Subject values are sanitized for the same reason.
// The delivery id (when the outbox set one) rides as X-Teploy-Delivery-Id so
// receivers can dedupe at-least-once redelivery (D08).
func buildEmailMessage(from, to string, event Event) string {
	subject := "[teploy] " + sanitizeHeader(Title(event))
	body := Body(event)

	headers := fmt.Sprintf("From: %s\r\nTo: %s\r\nSubject: %s\r\n",
		sanitizeHeader(from), sanitizeHeader(to), subject)
	if event.DeliveryID != "" {
		headers += fmt.Sprintf("X-Teploy-Delivery-Id: %s\r\n", sanitizeHeader(event.DeliveryID))
	}
	return headers + "\r\n" + body
}

// sendMailTimeout is a deadline-bounded replacement for smtp.SendMail so a hung
// mail server can't leak a goroutine on every monitor flap. The deadline covers
// the whole dial+exchange; a watchdog closes the conn on expiry so even a
// post-greeting hang unblocks.
//
// A45: transport expectations are explicit instead of opportunistic.
// STARTTLS is REQUIRED unless the caller explicitly opted into plaintext
// (Config.SMTPAllowInsecure, for trusted LAN relays); when credentials are
// configured but the server offers no AUTH, delivery fails instead of
// silently sending unauthenticated.
func sendMailTimeout(addr, host string, auth smtp.Auth, from string, to []string, msg []byte, timeout time.Duration, allowInsecure bool) error {
	conn, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		return err
	}
	deadline := time.Now().Add(timeout)
	_ = conn.SetDeadline(deadline)

	c, err := smtp.NewClient(conn, host)
	if err != nil {
		conn.Close()
		return err
	}
	defer c.Close()

	// Watchdog: close the conn if the exchange overruns the deadline, so a hang
	// after the greeting still returns instead of blocking forever.
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-done:
		case <-time.After(time.Until(deadline)):
			conn.Close()
		}
	}()

	if ok, _ := c.Extension("STARTTLS"); ok {
		if err := c.StartTLS(&tls.Config{ServerName: host, MinVersion: tls.VersionTLS12}); err != nil {
			return err
		}
	} else if !allowInsecure {
		return errors.New("SMTP server does not offer required STARTTLS (set smtp_allow_insecure for a trusted LAN relay)")
	}
	if auth != nil {
		if ok, _ := c.Extension("AUTH"); !ok {
			return errors.New("SMTP authentication configured but the server offers no AUTH")
		}
		if err := c.Auth(auth); err != nil {
			return err
		}
	}
	if err := c.Mail(from); err != nil {
		return err
	}
	for _, rcpt := range to {
		if err := c.Rcpt(rcpt); err != nil {
			return err
		}
	}
	w, err := c.Data()
	if err != nil {
		return err
	}
	if _, err := w.Write(msg); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	return c.Quit()
}

// sanitizeHeader removes CR/LF so a value can be safely placed in an email
// header line without injecting additional headers.
func sanitizeHeader(s string) string {
	return strings.NewReplacer("\r", "", "\n", "").Replace(s)
}
