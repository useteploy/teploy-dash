package alert

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
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
var webhookClient = &http.Client{Timeout: 10 * time.Second}

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
}

// Event represents a monitor state change.
type Event struct {
	MonitorID   string    `json:"monitor_id"`
	MonitorName string    `json:"monitor_name"`
	Status      string    `json:"status"` // "down" or "up" (recovery)
	Message     string    `json:"message"`
	OccurredAt  time.Time `json:"occurred_at"`
}

// Dispatcher sends alerts via configured channels.
type Dispatcher struct {
	config Config
}

// New creates an alert dispatcher.
func New(config Config) *Dispatcher {
	return &Dispatcher{config: config}
}

// Send dispatches an alert event to all configured channels.
func (d *Dispatcher) Send(event Event) {
	if d.config.WebhookURL != "" {
		go d.sendWebhook(event)
	}
	if d.config.SMTPHost != "" && d.config.EmailTo != "" {
		go d.sendEmail(event)
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

func (d *Dispatcher) sendWebhook(event Event) {
	payload, err := json.Marshal(event)
	if err != nil {
		log.Printf("[alert] Failed to marshal webhook payload: %v", err)
		return
	}

	req, err := http.NewRequest(http.MethodPost, d.config.WebhookURL, bytes.NewReader(payload))
	if err != nil {
		log.Printf("[alert] Webhook request build failed: %v", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	signWebhook(req, d.config.WebhookSecret, payload)

	resp, err := webhookClient.Do(req)
	if err != nil {
		log.Printf("[alert] Webhook failed: %v", err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		log.Printf("[alert] Webhook returned %d", resp.StatusCode)
	}
}

func (d *Dispatcher) sendEmail(event Event) {
	// Strip CR/LF from values that land in the Subject header so a monitor name
	// can't inject additional SMTP headers (e.g. an unwanted Bcc).
	subject := fmt.Sprintf("[teploy] %s is %s",
		sanitizeHeader(event.MonitorName), sanitizeHeader(event.Status))
	body := fmt.Sprintf("Monitor: %s\nStatus: %s\nMessage: %s\nTime: %s",
		event.MonitorName, event.Status, event.Message, event.OccurredAt.Format(time.RFC3339))

	msg := fmt.Sprintf("From: %s\r\nTo: %s\r\nSubject: %s\r\n\r\n%s",
		d.config.EmailFrom, d.config.EmailTo, subject, body)

	addr := fmt.Sprintf("%s:%d", d.config.SMTPHost, d.config.SMTPPort)
	var auth smtp.Auth
	if d.config.SMTPUser != "" {
		auth = smtp.PlainAuth("", d.config.SMTPUser, d.config.SMTPPass, d.config.SMTPHost)
	}

	if err := sendMailTimeout(addr, d.config.SMTPHost, auth, d.config.EmailFrom, []string{d.config.EmailTo}, []byte(msg), 10*time.Second); err != nil {
		log.Printf("[alert] Email failed: %v", err)
	}
}

// sendMailTimeout is a deadline-bounded replacement for smtp.SendMail so a hung
// mail server can't leak a goroutine on every monitor flap. The deadline covers
// the whole dial+exchange; a watchdog closes the conn on expiry so even a
// post-greeting hang unblocks.
func sendMailTimeout(addr, host string, auth smtp.Auth, from string, to []string, msg []byte, timeout time.Duration) error {
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
		if err := c.StartTLS(&tls.Config{ServerName: host}); err != nil {
			return err
		}
	}
	if auth != nil {
		if ok, _ := c.Extension("AUTH"); ok {
			if err := c.Auth(auth); err != nil {
				return err
			}
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
