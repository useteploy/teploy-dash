package alert

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
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

// Event sources (D08): the label on an Event naming what produced it.
// Monitors and restore tests share one alert outbox; the label scopes
// delivery-status lookups and lets receivers route. Empty is legacy
// monitor events (pre-label journals decode as monitors).
const (
	SourceMonitor     = "monitor"
	SourceRestoreTest = "restore-test"
)

// Config holds alerting configuration.
type Config struct {
	WebhookURL string `json:"webhook_url,omitempty"`
	// WebhookSecret signs deliveries so the receiver can tell a real alert from
	// anyone who learned the URL. Empty sends unsigned, as before.
	WebhookSecret string `json:"webhook_secret,omitempty"`
	SMTPHost      string `json:"smtp_host,omitempty"`
	SMTPPort      int    `json:"smtp_port,omitempty"`
	SMTPUser      string `json:"smtp_user,omitempty"`
	SMTPPass      string `json:"smtp_pass,omitempty"`
	EmailTo       string `json:"email_to,omitempty"`
	EmailFrom     string `json:"email_from,omitempty"`
	// SMTPAllowInsecure opts this installation into plaintext SMTP for
	// trusted LAN relays. Without it, delivery REQUIRES STARTTLS and fails
	// loudly when the server doesn't offer it — the old code downgraded
	// silently, sending alert payloads (and auth) in the clear (A45).
	SMTPAllowInsecure bool `json:"smtp_allow_insecure,omitempty"`
}

// Channel names for per-channel delivery accounting (D08). A Config's
// configured channel set is exactly these, in canonical order.
const (
	ChannelWebhook = "webhook"
	ChannelEmail   = "email"
)

// ConfiguredChannels lists the channels a config actually delivers through:
// the webhook when a URL is set, email when host AND recipient are set (the
// same predicates SendSync has always gated on — one definition, no drift).
func ConfiguredChannels(c Config) []string {
	var out []string
	if c.WebhookURL != "" {
		out = append(out, ChannelWebhook)
	}
	if c.SMTPHost != "" && c.EmailTo != "" {
		out = append(out, ChannelEmail)
	}
	return out
}

// Event represents a monitor state change.
type Event struct {
	MonitorID   string    `json:"monitor_id"`
	MonitorName string    `json:"monitor_name"`
	Status      string    `json:"status"` // "down" or "up" (recovery)
	Message     string    `json:"message"`
	OccurredAt  time.Time `json:"occurred_at"`
	// Source labels what produced the event — "monitor" or "restore-test"
	// (D08: both ride the alert outbox; the label scopes delivery-status
	// lookups and lets receivers route). Empty is legacy monitor events.
	Source string `json:"source,omitempty"`
	// DeliveryID is the outbox's idempotency key for this delivery (D08:
	// per-channel accounting). Empty on direct (non-outbox) sends; when set
	// it travels in the webhook payload and as an email header so a receiver
	// can dedupe the at-least-once redelivery the outbox permits.
	DeliveryID string `json:"delivery_id,omitempty"`
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
// failures are logged, not retried, not surfaced anywhere (the pre-D08
// contract, kept for callers that only want fire-and-forget — the monitor
// runner goes through the durable outbox instead).
func (d *Dispatcher) Send(event Event) {
	if d.config.WebhookURL != "" || (d.config.SMTPHost != "" && d.config.EmailTo != "") {
		go func() {
			if err := d.SendSync(event); err != nil {
				log.Printf("[alert] delivery failed: %v", err)
			}
		}()
	}
}

// SendSync delivers to every configured channel synchronously and reports
// the combined failure (D08: the outbox needs a verdict per attempt, not a
// fire-and-forget goroutine). A nil return means every configured channel
// acknowledged the delivery.
func (d *Dispatcher) SendSync(event Event) error {
	var errs []error
	for _, channel := range ConfiguredChannels(d.config) {
		if err := d.SendChannel(event, channel); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", channel, err))
		}
	}
	return errors.Join(errs...)
}

// SendChannel delivers through exactly one named channel (D08 per-channel
// accounting: the outbox retries only the channels that failed, so a webhook
// that acknowledged is never re-sent because the email leg failed). An
// unknown or unconfigured channel name is an error, never a silent skip.
func (d *Dispatcher) SendChannel(event Event, channel string) error {
	switch channel {
	case ChannelWebhook:
		if d.config.WebhookURL == "" {
			return errors.New("webhook channel is not configured")
		}
		return d.sendWebhook(event)
	case ChannelEmail:
		if d.config.SMTPHost == "" || d.config.EmailTo == "" {
			return errors.New("email channel is not configured")
		}
		return d.sendEmail(event)
	default:
		return fmt.Errorf("unknown alert channel %q", channel)
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

func (d *Dispatcher) sendWebhook(event Event) error {
	payload, err := json.Marshal(event)
	if err != nil {
		return err
	}

	req, err := http.NewRequest(http.MethodPost, d.config.WebhookURL, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	signWebhook(req, d.config.WebhookSecret, payload)

	resp, err := webhookClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		// 3xx included: a redirect is a non-delivery, not a success —
		// the receiver never saw the body (see webhookClient).
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return nil
}

func (d *Dispatcher) sendEmail(event Event) error {
	msg := buildEmailMessage(d.config.EmailFrom, d.config.EmailTo, event)

	addr := net.JoinHostPort(d.config.SMTPHost, strconv.Itoa(d.config.SMTPPort))
	var auth smtp.Auth
	if d.config.SMTPUser != "" {
		auth = smtp.PlainAuth("", d.config.SMTPUser, d.config.SMTPPass, d.config.SMTPHost)
	}

	return sendMailTimeout(addr, d.config.SMTPHost, auth, d.config.EmailFrom, []string{d.config.EmailTo}, []byte(msg), 10*time.Second, d.config.SMTPAllowInsecure)
}

// buildEmailMessage constructs the raw RFC 5322 message for an alert email.
// from and to are sanitized (CR/LF stripped) before being placed in headers so
// a configured email_from/email_to can't inject additional SMTP headers
// (e.g. an unwanted Bcc). Subject values are sanitized for the same reason.
// The delivery id (when the outbox set one) rides as X-Teploy-Delivery-Id so
// receivers can dedupe at-least-once redelivery (D08).
func buildEmailMessage(from, to string, event Event) string {
	subject := fmt.Sprintf("[teploy] %s is %s",
		sanitizeHeader(event.MonitorName), sanitizeHeader(event.Status))
	body := fmt.Sprintf("Monitor: %s\nStatus: %s\nMessage: %s\nTime: %s",
		event.MonitorName, event.Status, event.Message, event.OccurredAt.Format(time.RFC3339))

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
