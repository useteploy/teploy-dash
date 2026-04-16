package alert

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/smtp"
	"time"
)

// Config holds alerting configuration.
type Config struct {
	WebhookURL string `json:"webhook_url,omitempty"`
	SMTPHost   string `json:"smtp_host,omitempty"`
	SMTPPort   int    `json:"smtp_port,omitempty"`
	SMTPUser   string `json:"smtp_user,omitempty"`
	SMTPPass   string `json:"smtp_pass,omitempty"`
	EmailTo    string `json:"email_to,omitempty"`
	EmailFrom  string `json:"email_from,omitempty"`
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

func (d *Dispatcher) sendWebhook(event Event) {
	payload, err := json.Marshal(event)
	if err != nil {
		log.Printf("[alert] Failed to marshal webhook payload: %v", err)
		return
	}

	resp, err := http.Post(d.config.WebhookURL, "application/json", bytes.NewReader(payload))
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
	subject := fmt.Sprintf("[teploy] %s is %s", event.MonitorName, event.Status)
	body := fmt.Sprintf("Monitor: %s\nStatus: %s\nMessage: %s\nTime: %s",
		event.MonitorName, event.Status, event.Message, event.OccurredAt.Format(time.RFC3339))

	msg := fmt.Sprintf("From: %s\r\nTo: %s\r\nSubject: %s\r\n\r\n%s",
		d.config.EmailFrom, d.config.EmailTo, subject, body)

	addr := fmt.Sprintf("%s:%d", d.config.SMTPHost, d.config.SMTPPort)
	var auth smtp.Auth
	if d.config.SMTPUser != "" {
		auth = smtp.PlainAuth("", d.config.SMTPUser, d.config.SMTPPass, d.config.SMTPHost)
	}

	if err := smtp.SendMail(addr, auth, d.config.EmailFrom, []string{d.config.EmailTo}, []byte(msg)); err != nil {
		log.Printf("[alert] Email failed: %v", err)
	}
}
