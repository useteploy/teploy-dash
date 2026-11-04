package alert

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

// Channel types (Alerts v2, plan P3.4). webhook and ship are MACHINE
// channels: they receive the signed Event v2 JSON body. slack, discord, ntfy
// and email are HUMAN channels: they receive a rendered text message.
const (
	TypeWebhook = "webhook"
	TypeSlack   = "slack"
	TypeDiscord = "discord"
	TypeNtfy    = "ntfy"
	TypeEmail   = "email"
	TypeShip    = "ship"
)

// Legacy channel ids (D08 ledger keys). A pre-v2 notifications.json with
// webhook_url / smtp_* migrates into channels with exactly these ids, so
// outbox records written before the migration replay against the same
// ledger keys.
const (
	ChannelWebhook = "webhook"
	ChannelEmail   = "email"
)

// Event kinds (Event v2 wire contract, plan 2.5).
const (
	KindOpened    = "incident_opened"
	KindRepeat    = "incident_repeat"
	KindRecovered = "incident_recovered"
	KindTest      = "test"
	// KindSummary is INTERNAL: the first-enable summary is one rendered
	// message for human channels and never reaches a machine channel's
	// wire body (machine channels receive the per-incident opens instead).
	KindSummary = "summary"
)

// Recovery reasons (plan 2.5).
const (
	ReasonHealthy       = ""
	ReasonRetired       = "retired"
	ReasonSuppressedEnd = "suppressed_end"
)

// Audiences restrict which channel class an event is for (internal).
const (
	AudienceAll     = ""
	AudienceHuman   = "human"
	AudienceMachine = "machine"
)

var channelIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,47}$`)

// ValidChannelID reports whether id is a usable channel id (it is a ledger
// key and a URL path segment).
func ValidChannelID(id string) bool { return channelIDPattern.MatchString(id) }

// Channel is one alert destination.
type Channel struct {
	ID   string `json:"id"`
	Type string `json:"type"`
	Name string `json:"name,omitempty"`
	// URL is the destination: the webhook / Slack-compatible / Discord
	// webhook URL, the ntfy topic URL (<server>/<topic>), or the Ship intake
	// URL. Unused for email.
	URL string `json:"url,omitempty"`
	// Secret signs webhook and ship deliveries (X-Teploy-Signature).
	Secret string `json:"secret,omitempty"`
	// Token is the ntfy bearer token (optional).
	Token string `json:"token,omitempty"`
	// Kinds restricts the event kinds this channel receives; empty = all.
	// Test sends always reach the channel they target.
	Kinds []string `json:"kinds,omitempty"`

	// Email (SMTP) settings, used only by type email.
	SMTPHost          string `json:"smtp_host,omitempty"`
	SMTPPort          int    `json:"smtp_port,omitempty"`
	SMTPUser          string `json:"smtp_user,omitempty"`
	SMTPPass          string `json:"smtp_pass,omitempty"`
	EmailTo           string `json:"email_to,omitempty"`
	EmailFrom         string `json:"email_from,omitempty"`
	SMTPAllowInsecure bool   `json:"smtp_allow_insecure,omitempty"`
	// AllowInternal lets this channel reach loopback/private/CGNAT
	// (tailnet) addresses — the monitor network guard's opt-in, for a
	// receiver on your own network (an Akiroo or ntfy over Tailscale).
	AllowInternal bool `json:"allow_internal,omitempty"`
}

// Human reports whether the channel receives rendered text (not the JSON
// contract).
func (c Channel) Human() bool {
	switch c.Type {
	case TypeSlack, TypeDiscord, TypeNtfy, TypeEmail:
		return true
	}
	return false
}

// Usable reports whether the channel has what it needs to deliver. A
// half-configured channel is not counted as configured (the predicate
// SendSync has always gated on for the legacy pair).
func (c Channel) Usable() bool {
	switch c.Type {
	case TypeEmail:
		return c.SMTPHost != "" && c.EmailTo != ""
	case TypeWebhook, TypeSlack, TypeDiscord, TypeNtfy:
		return c.URL != ""
	case TypeShip:
		return c.URL != "" && c.Secret != ""
	}
	return false
}

// HeartbeatTarget is one dead-man receiver Dash pings (plan P3.7).
type HeartbeatTarget struct {
	ID   string `json:"id"`
	Name string `json:"name,omitempty"`
	URL  string `json:"url"`
	// AllowInternal opts this receiver out of the network guard (another
	// Dash or a Kuma on your tailnet).
	AllowInternal bool `json:"allow_internal,omitempty"`
}

// Config is the whole notifications.json document.
type Config struct {
	Channels []Channel `json:"channels,omitempty"`
	// PublicURL is Dash's externally reachable base URL, used for links in
	// alerts ("" = no links).
	PublicURL string `json:"public_url,omitempty"`
	// Heartbeats are the outbound self-heartbeat ping URLs.
	Heartbeats               []HeartbeatTarget `json:"heartbeats,omitempty"`
	HeartbeatIntervalMinutes int               `json:"heartbeat_interval_minutes,omitempty"`

	// Legacy single-webhook / single-SMTP fields (pre-v2). Read for
	// migration only: Migrate moves them into Channels with ids "webhook"
	// and "email"; a saved config never carries them again.
	WebhookURL        string `json:"webhook_url,omitempty"`
	WebhookSecret     string `json:"webhook_secret,omitempty"`
	SMTPHost          string `json:"smtp_host,omitempty"`
	SMTPPort          int    `json:"smtp_port,omitempty"`
	SMTPUser          string `json:"smtp_user,omitempty"`
	SMTPPass          string `json:"smtp_pass,omitempty"`
	EmailTo           string `json:"email_to,omitempty"`
	EmailFrom         string `json:"email_from,omitempty"`
	SMTPAllowInsecure bool   `json:"smtp_allow_insecure,omitempty"`
}

// HasLegacy reports whether any pre-v2 field is still set.
func (c Config) HasLegacy() bool {
	return c.WebhookURL != "" || c.WebhookSecret != "" || c.SMTPHost != "" || c.SMTPPort != 0 ||
		c.SMTPUser != "" || c.SMTPPass != "" || c.EmailTo != "" || c.EmailFrom != "" || c.SMTPAllowInsecure
}

// Migrate returns the config with legacy fields moved into channels "webhook"
// and "email" (when no channel already owns that id) and cleared. Pure:
// callers that persist decide when to write the result.
func (c Config) Migrate() Config {
	out := c
	out.Channels = append([]Channel(nil), c.Channels...)
	has := func(id string) bool {
		for _, ch := range out.Channels {
			if ch.ID == id {
				return true
			}
		}
		return false
	}
	var legacy []Channel
	if c.WebhookURL != "" && !has(ChannelWebhook) {
		legacy = append(legacy, Channel{ID: ChannelWebhook, Type: TypeWebhook, Name: "Webhook", URL: c.WebhookURL, Secret: c.WebhookSecret})
	}
	if (c.SMTPHost != "" || c.EmailTo != "") && !has(ChannelEmail) {
		legacy = append(legacy, Channel{ID: ChannelEmail, Type: TypeEmail, Name: "Email",
			SMTPHost: c.SMTPHost, SMTPPort: c.SMTPPort, SMTPUser: c.SMTPUser, SMTPPass: c.SMTPPass,
			EmailTo: c.EmailTo, EmailFrom: c.EmailFrom, SMTPAllowInsecure: c.SMTPAllowInsecure})
	}
	// Migrated channels lead, in the canonical legacy order.
	out.Channels = append(legacy, out.Channels...)
	out.WebhookURL, out.WebhookSecret = "", ""
	out.SMTPHost, out.SMTPPort, out.SMTPUser, out.SMTPPass = "", 0, "", ""
	out.EmailTo, out.EmailFrom, out.SMTPAllowInsecure = "", "", false
	return out
}

// Channel returns the effective channel with the given id.
func (c Config) Channel(id string) (Channel, bool) {
	for _, ch := range c.Migrate().Channels {
		if ch.ID == id {
			return ch, true
		}
	}
	return Channel{}, false
}

// ConfiguredChannels lists the ids of every usable channel, in config order.
func ConfiguredChannels(c Config) []string {
	var out []string
	for _, ch := range c.Migrate().Channels {
		if ch.Usable() {
			out = append(out, ch.ID)
		}
	}
	return out
}

// ChannelsFor lists the usable channel ids that should receive ev: the
// event's own routing (only/exclude/audience) and each channel's kind filter.
func ChannelsFor(c Config, ev Event) []string {
	kind := ev.wireKind()
	var out []string
	for _, ch := range c.Migrate().Channels {
		if !ch.Usable() {
			continue
		}
		if len(ev.OnlyChannels) > 0 && !contains(ev.OnlyChannels, ch.ID) {
			continue
		}
		if contains(ev.ExcludeChannels, ch.ID) {
			continue
		}
		switch ev.Audience {
		case AudienceHuman:
			if !ch.Human() {
				continue
			}
		case AudienceMachine:
			if ch.Human() {
				continue
			}
		}
		if kind != KindTest && len(ch.Kinds) > 0 {
			want := kind
			if kind == KindSummary {
				want = KindOpened
			}
			if !contains(ch.Kinds, want) {
				continue
			}
		}
		out = append(out, ch.ID)
	}
	return out
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// ValidKind reports whether k may appear in a channel's kinds filter.
func ValidKind(k string) bool {
	return k == KindOpened || k == KindRepeat || k == KindRecovered
}

// ValidateChannel checks one channel's shape at the API boundary.
func ValidateChannel(ch Channel) error {
	if !ValidChannelID(ch.ID) {
		return fmt.Errorf("channel id must be lowercase letters, digits, '-' or '_' (max 48)")
	}
	if len(ch.Name) > 80 {
		return fmt.Errorf("channel name is too long")
	}
	for _, k := range ch.Kinds {
		if !ValidKind(k) {
			return fmt.Errorf("unknown event kind %q (use incident_opened, incident_repeat, incident_recovered)", k)
		}
	}
	switch ch.Type {
	case TypeEmail:
		if ch.SMTPHost == "" || ch.EmailTo == "" {
			return fmt.Errorf("email channel needs an SMTP host and a recipient")
		}
		if ch.SMTPPort < 0 || ch.SMTPPort > 65535 {
			return fmt.Errorf("SMTP port must be between 0 and 65535")
		}
		return nil
	case TypeWebhook, TypeSlack, TypeDiscord, TypeNtfy, TypeShip:
	default:
		return fmt.Errorf("channel type must be webhook, slack, discord, ntfy, email or ship")
	}
	if err := ValidateURL(ch.URL); err != nil {
		return err
	}
	if ch.Type == TypeShip && ch.Secret == "" {
		return fmt.Errorf("a ship channel needs the intake signing secret")
	}
	return nil
}

// ValidateURL requires an absolute http(s) URL without embedded credentials
// (credentials belong in the secret/token fields, which GET never returns).
func ValidateURL(raw string) error {
	if strings.TrimSpace(raw) == "" {
		return fmt.Errorf("a URL is required")
	}
	if len(raw) > 2048 {
		return fmt.Errorf("URL is too long")
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
		return fmt.Errorf("URL must be an absolute http(s) URL")
	}
	if u.User != nil {
		return fmt.Errorf("URL must not embed credentials; use the secret or token field")
	}
	return nil
}

// URLHint renders a URL without its path and query: enough to recognise the
// destination, never the credential a webhook path usually is.
func URLHint(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return ""
	}
	if u.Path == "" || u.Path == "/" {
		return u.Scheme + "://" + u.Host
	}
	return u.Scheme + "://" + u.Host + "/..."
}

// URLIsCredential reports whether a channel type's URL is itself the secret
// (Slack-compatible and Discord webhook URLs authorize whoever holds them).
func URLIsCredential(typ string) bool {
	return typ == TypeSlack || typ == TypeDiscord
}
