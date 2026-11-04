package alert

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// RetryAfterError is a delivery refused with an explicit back-off hint
// (HTTP 429/503 Retry-After, Discord's retry_after). The outbox schedules
// the next attempt no earlier than After.
type RetryAfterError struct {
	After time.Duration
	Err   error
}

func (e *RetryAfterError) Error() string {
	return fmt.Sprintf("%v (retry after %s)", e.Err, e.After.Round(time.Second))
}

func (e *RetryAfterError) Unwrap() error { return e.Err }

// RetryAfter extracts the back-off hint from a delivery error (0 = none).
func RetryAfter(err error) time.Duration {
	var ra *RetryAfterError
	if errors.As(err, &ra) {
		return ra.After
	}
	return 0
}

// maxRetryAfter caps a receiver's hint so a hostile or buggy value cannot
// park an alert for days.
const maxRetryAfter = time.Hour

// parseRetryAfter reads a Retry-After header: delta-seconds or an HTTP date.
func parseRetryAfter(v string, now time.Time) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	var d time.Duration
	if secs, err := strconv.ParseFloat(v, 64); err == nil && secs > 0 {
		d = time.Duration(secs * float64(time.Second))
	} else if t, err := http.ParseTime(v); err == nil {
		d = t.Sub(now)
	}
	if d <= 0 {
		return 0
	}
	if d > maxRetryAfter {
		d = maxRetryAfter
	}
	return d
}

// post sends req and classifies the answer: 2xx is delivered, anything
// else (redirects included — see webhookClient) is a failure, with the
// Retry-After hint attached on 429/503. extraRetry parses a body-carried
// hint (Discord).
func post(req *http.Request, extraRetry func([]byte) time.Duration) error {
	resp, err := webhookClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 300 && resp.StatusCode >= 200 {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		return nil
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	failure := fmt.Errorf("HTTP %d", resp.StatusCode)
	if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusServiceUnavailable {
		after := parseRetryAfter(resp.Header.Get("Retry-After"), time.Now())
		if extraRetry != nil {
			if b := extraRetry(raw); b > after {
				after = b
			}
		}
		if after > 0 {
			return &RetryAfterError{After: after, Err: failure}
		}
	}
	return failure
}

func newJSONRequest(target string, payload []byte) (*http.Request, error) {
	req, err := http.NewRequest(http.MethodPost, target, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "teploy-dash")
	return req, nil
}

// sendWebhook delivers the signed Event v2 body (generic webhook and Ship
// intake): X-Teploy-Timestamp/Signature when a secret is set, plus
// X-Teploy-Delivery and X-Teploy-Event-Version: 2 on every delivery.
func sendWebhook(ch Channel, event Event) error {
	payload, err := json.Marshal(event.Wire())
	if err != nil {
		return err
	}
	req, err := newJSONRequest(ch.URL, payload)
	if err != nil {
		return err
	}
	req.Header.Set("X-Teploy-Event-Version", "2")
	if event.DeliveryID != "" {
		req.Header.Set("X-Teploy-Delivery", event.DeliveryID)
	}
	signWebhook(req, ch.Secret, payload)
	return post(req, nil)
}

// sendSlack delivers {"text": ...}: the Slack incoming-webhook shape, which
// Mattermost, Rocket.Chat and Google Chat accept unchanged.
func sendSlack(ch Channel, event Event) error {
	text := Title(event) + "\n" + Body(event)
	payload, err := json.Marshal(map[string]string{"text": text})
	if err != nil {
		return err
	}
	req, err := newJSONRequest(ch.URL, payload)
	if err != nil {
		return err
	}
	return post(req, nil)
}

// Discord limits: message content 2000 chars, embed title 256, description
// 4096 — the description is held to 2000 so the whole message reads like one
// chat message and stays well inside every limit.
const (
	discordTitleMax = 256
	discordDescMax  = 2000
)

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-3]) + "..."
}

func discordColor(event Event) int {
	switch {
	case event.wireKind() == KindTest:
		return 0x5865F2
	case event.Status == "up":
		return 0x3FB950
	case event.Severity == SeverityWarning:
		return 0xD29922
	default:
		return 0xF85149
	}
}

func sendDiscord(ch Channel, event Event) error {
	embed := map[string]any{
		"title":       truncate(Title(event), discordTitleMax),
		"description": truncate(Body(event), discordDescMax),
		"color":       discordColor(event),
	}
	if !event.OccurredAt.IsZero() {
		embed["timestamp"] = event.OccurredAt.UTC().Format(time.RFC3339)
	}
	if event.URL != "" {
		embed["url"] = event.URL
	}
	payload, err := json.Marshal(map[string]any{
		"embeds":           []any{embed},
		"allowed_mentions": map[string]any{"parse": []string{}},
	})
	if err != nil {
		return err
	}
	req, err := newJSONRequest(ch.URL, payload)
	if err != nil {
		return err
	}
	return post(req, func(body []byte) time.Duration {
		var rl struct {
			RetryAfter float64 `json:"retry_after"`
		}
		if json.Unmarshal(body, &rl) != nil || rl.RetryAfter <= 0 {
			return 0
		}
		d := time.Duration(rl.RetryAfter * float64(time.Second))
		if d > maxRetryAfter {
			d = maxRetryAfter
		}
		return d
	})
}

// ntfyTags are PLAIN WORDS. ntfy renders known tag names as emoji; the
// product never ships emoji, so no tag here is an emoji shortcode name.
func ntfyTags(event Event) string {
	tags := []string{"teploy"}
	switch {
	case event.wireKind() == KindTest:
		tags = append(tags, "test")
	case event.Status == "up":
		tags = append(tags, "recovered")
	default:
		tags = append(tags, "down")
	}
	src := event.Source
	if src == "" {
		src = SourceMonitor
	}
	tags = append(tags, src)
	return strings.Join(tags, ",")
}

func ntfyPriority(event Event) string {
	if event.Status == "up" || event.wireKind() == KindTest {
		return "default"
	}
	if event.Severity == SeverityWarning || event.Severity == SeverityInfo {
		return "high"
	}
	return "urgent"
}

// sendNtfy POSTs the text body to <server>/<topic> with ntfy's headers.
func sendNtfy(ch Channel, event Event, link string) error {
	req, err := http.NewRequest(http.MethodPost, ch.URL, strings.NewReader(Body(event)))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "text/plain; charset=utf-8")
	req.Header.Set("User-Agent", "teploy-dash")
	req.Header.Set("Title", sanitizeHeader(Title(event)))
	req.Header.Set("Priority", ntfyPriority(event))
	req.Header.Set("Tags", ntfyTags(event))
	if link != "" {
		req.Header.Set("Click", sanitizeHeader(link))
		req.Header.Set("Actions", "view, Open in Dash, "+sanitizeHeader(link))
	}
	if ch.Token != "" {
		req.Header.Set("Authorization", "Bearer "+sanitizeHeader(ch.Token))
	}
	return post(req, nil)
}

// Title is the one-line human headline for an event.
func Title(e Event) string {
	name := e.MonitorName
	switch e.wireKind() {
	case KindTest:
		return "Test notification from Teploy Dash"
	case KindSummary:
		return name
	case KindRepeat:
		return "Still down: " + name
	case KindRecovered:
		switch e.Reason {
		case ReasonRetired:
			return "Resolved (retired): " + name
		case ReasonSuppressedEnd:
			return "Resolved (silenced): " + name
		}
		return "Recovered: " + name
	default:
		return "Down: " + name
	}
}

// Body is the multi-line human message for an event (plain text, no emoji).
func Body(e Event) string {
	var b strings.Builder
	line := func(s string) {
		if s != "" {
			b.WriteString(s)
			b.WriteString("\n")
		}
	}
	line(e.Message)
	if e.wireKind() == KindSummary {
		line(e.URL)
		return strings.TrimRight(b.String(), "\n")
	}
	src := e.Source
	if src == "" {
		src = SourceMonitor
	}
	where := "Source: " + src
	if e.App != "" {
		where += ", app " + e.App
	}
	if e.Server != "" {
		where += " on " + e.Server
	}
	line(where)
	if e.Target != "" {
		line("Target: " + e.Target)
	}
	if !e.StartedAt.IsZero() && e.wireKind() != KindTest {
		since := "Since: " + e.StartedAt.UTC().Format(time.RFC3339)
		if e.DurationS > 0 {
			since += " (" + (time.Duration(e.DurationS) * time.Second).String() + ")"
		}
		line(since)
	}
	if e.ConsecutiveFailures > 0 && e.Status != "up" {
		line(fmt.Sprintf("Consecutive failures: %d", e.ConsecutiveFailures))
	}
	if !e.OccurredAt.IsZero() {
		line("Time: " + e.OccurredAt.UTC().Format(time.RFC3339))
	}
	line(e.URL)
	return strings.TrimRight(b.String(), "\n")
}
