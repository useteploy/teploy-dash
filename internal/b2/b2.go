// Package b2 is a small client for Backblaze's native v4 API: what "Protect my data"
// needs and nothing more. Standard library only. The base URL is injectable so tests
// run against a fake server.
//
// Nothing here logs, and an error never carries the store's message text, which can
// echo what was sent: it holds the status and the code, and says what they mean in
// plain words.
package b2

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// DefaultBaseURL is where every account is authorized.
const DefaultBaseURL = "https://api.backblazeb2.com"

const (
	maxReply              = 8 << 20
	maxUnavailableRetries = 2
	// codeWrongKey stands in for "unauthorized" on the authorize call itself.
	codeWrongKey = "wrong_key"
)

// Capabilities a setup key needs, and those it can make use of.
const (
	CapListBuckets  = "listBuckets"
	CapWriteBuckets = "writeBuckets"
	CapWriteKeys    = "writeKeys"
	CapDeleteKeys   = "deleteKeys"
	CapListFiles    = "listFiles"
	CapDeleteFiles  = "deleteFiles"
	CapReadFiles    = "readFiles"
	CapWriteFiles   = "writeFiles"
	// CapReadLifecycle lets a server read the bucket's rules. Backblaze's capability
	// list does not always name it, so a refusal is handled and reported.
	CapReadLifecycle = "readBucketLifecycleRules"
)

// RequiredSetupCapabilities is what a key must hold for Dash to set up a bucket.
var RequiredSetupCapabilities = []string{CapListBuckets, CapWriteBuckets, CapWriteKeys}

// Client talks to one base URL.
type Client struct {
	BaseURL string
	HTTP    *http.Client
	// AnyHost allows API hosts other than *.backblazeb2.com over any scheme (tests).
	AnyHost bool
	// RetryWait is the first wait after a 503, doubling each time. Zero means a second.
	RetryWait time.Duration
}

// APIError is a refusal from Backblaze. The store's message is read only to classify
// it, and never leaves this package.
type APIError struct {
	Status int
	Code   string
	// noSuchKey: the message says the key being acted on does not exist.
	noSuchKey bool
}

// Error is the plain wording, safe for a response.
func (e *APIError) Error() string { return plainMessage(e.Status, e.Code) }

func plainMessage(status int, code string) string {
	switch code {
	case "transaction_cap_exceeded":
		return "Backblaze's daily transaction cap on this account has been reached. Your data is fine. Raise the cap under Caps & Alerts in the Backblaze console, or try again tomorrow."
	case "email_not_verified":
		return "Backblaze needs this account's email address verified first. Verify it in the Backblaze console, then try again."
	case "no_payment_history":
		return "Backblaze needs a payment method on this account for this. Add one in the Backblaze console, then try again."
	case codeWrongKey:
		return "Backblaze did not accept that key. Check both parts were pasted whole, and that the key has not expired or been deleted."
	case "bad_auth_token", "expired_auth_token":
		return "Backblaze ended this session. Try again. If it happens again, paste a new key."
	case "unauthorized":
		return "This Backblaze key is not allowed to do that. Check its permissions in the Backblaze console."
	case "too_many_buckets":
		return "This Backblaze account already has the most buckets it can have. Choose an existing bucket, or delete one in the Backblaze console."
	case "bad_bucket_id":
		return "Backblaze does not know that bucket any more. Check the bucket and try again."
	case "file_lock_conflict":
		return "This bucket has Object Lock on, and Backblaze refused the change. Choose another bucket."
	case "conflict":
		return "Someone else changed this bucket at the same moment. Try again."
	case "duplicate_bucket_name":
		return "That bucket name is taken."
	}
	if code != "" {
		return fmt.Sprintf("Backblaze answered %d (%s).", status, code)
	}
	return fmt.Sprintf("Backblaze answered %d.", status)
}

// IsCode reports whether err is a refusal with this code.
func IsCode(err error, code string) bool {
	var e *APIError
	return errors.As(err, &e) && e.Code == code
}

// IsMissingKey reports whether a delete failed because the key is already gone. Any
// other bad_request leaves the key alive, and it must not be counted as deleted.
func IsMissingKey(err error) bool {
	var e *APIError
	return errors.As(err, &e) && e.noSuchKey
}

// IsCapped reports whether err is the daily transaction cap: not an outage, and never
// to be shown as one.
func IsCapped(err error) bool { return IsCode(err, "transaction_cap_exceeded") }

// BucketRef is a bucket a key is limited to.
type BucketRef struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// Session is an authorized key. The token is on it and goes nowhere else.
type Session struct {
	client       *Client
	token        string
	AccountID    string
	APIURL       string
	S3URL        string
	Region       string
	Capabilities []string
	// Buckets is set when the key is limited to these.
	Buckets    []BucketRef
	NamePrefix string
	// ExpiresAt is nil for a key that never expires.
	ExpiresAt *time.Time
}

// Has reports whether the key holds a capability.
func (s *Session) Has(capability string) bool {
	for _, c := range s.Capabilities {
		if c == capability {
			return true
		}
	}
	return false
}

// Missing lists required capabilities the key lacks.
func (s *Session) Missing() []string {
	out := []string{}
	for _, c := range RequiredSetupCapabilities {
		if !s.Has(c) {
			out = append(out, c)
		}
	}
	return out
}

var s3HostRE = regexp.MustCompile(`^s3\.([a-z0-9][a-z0-9-]{0,24})\.backblazeb2\.com$`)

func (c *Client) checkAPIURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil {
		return errors.New("Backblaze gave an API address that is not a URL")
	}
	if c.AnyHost {
		return nil
	}
	if u.Scheme != "https" || !strings.HasSuffix(u.Hostname(), ".backblazeb2.com") {
		return errors.New("Backblaze gave an API address outside backblazeb2.com")
	}
	return nil
}

// Authorize exchanges a key for a session.
func (c *Client) Authorize(ctx context.Context, keyID, appKey string) (*Session, error) {
	base := c.BaseURL
	if base == "" {
		base = DefaultBaseURL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(base, "/")+"/b2api/v4/b2_authorize_account", nil)
	if err != nil {
		return nil, errors.New("could not build the request to Backblaze")
	}
	req.SetBasicAuth(keyID, appKey)
	var reply struct {
		AccountID string `json:"accountId"`
		Token     string `json:"authorizationToken"`
		Expiry    *int64 `json:"applicationKeyExpirationTimestamp"`
		APIInfo   struct {
			Storage struct {
				APIURL  string `json:"apiUrl"`
				S3URL   string `json:"s3ApiUrl"`
				Allowed struct {
					Buckets      []BucketRef `json:"buckets"`
					Capabilities []string    `json:"capabilities"`
					NamePrefix   *string     `json:"namePrefix"`
				} `json:"allowed"`
			} `json:"storageApi"`
		} `json:"apiInfo"`
	}
	if err := c.send(func() (*http.Request, error) { return req.Clone(ctx), nil }, &reply); err != nil {
		// Here "unauthorized" means the key itself was refused, not that it lacks a
		// permission.
		var e *APIError
		if errors.As(err, &e) && e.Code == "unauthorized" {
			return nil, &APIError{Status: e.Status, Code: codeWrongKey}
		}
		return nil, err
	}
	st := reply.APIInfo.Storage
	if reply.AccountID == "" || reply.Token == "" || st.APIURL == "" || st.S3URL == "" || st.Allowed.Capabilities == nil {
		return nil, errors.New("Backblaze's reply to the key was incomplete")
	}
	if err := c.checkAPIURL(st.APIURL); err != nil {
		return nil, err
	}
	u, err := url.Parse(st.S3URL)
	if err != nil {
		return nil, errors.New("Backblaze gave an S3 address that is not a URL")
	}
	m := s3HostRE.FindStringSubmatch(u.Hostname())
	if m == nil || u.Scheme != "https" || u.Port() != "" {
		return nil, errors.New("Backblaze gave an S3 address that does not look like one of its own")
	}
	s := &Session{client: c, token: reply.Token, AccountID: reply.AccountID,
		APIURL: strings.TrimRight(st.APIURL, "/"), S3URL: strings.TrimRight(st.S3URL, "/"), Region: m[1],
		Capabilities: st.Allowed.Capabilities}
	for _, b := range st.Allowed.Buckets {
		if b.ID != "" {
			s.Buckets = append(s.Buckets, b)
		}
	}
	if st.Allowed.NamePrefix != nil {
		s.NamePrefix = *st.Allowed.NamePrefix
	}
	if reply.Expiry != nil && *reply.Expiry > 0 {
		t := time.UnixMilli(*reply.Expiry).UTC()
		s.ExpiresAt = &t
	}
	return s, nil
}

func (c *Client) httpClient() *http.Client {
	base := c.HTTP
	if base == nil {
		base = &http.Client{Timeout: 45 * time.Second}
	}
	client := *base
	// A redirect would carry the token or the key somewhere else.
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &client
}

// send retries a 503, which Backblaze's docs call a temporary state to wait out. Nothing
// else is retried: a create that may have landed must not run twice.
func (c *Client) send(build func() (*http.Request, error), out any) error {
	wait := c.RetryWait
	if wait == 0 {
		wait = time.Second
	}
	var data []byte
	for attempt := 0; ; attempt++ {
		req, err := build()
		if err != nil {
			return errors.New("could not build the request to Backblaze")
		}
		resp, err := c.httpClient().Do(req)
		if err != nil {
			var ue *url.Error
			if errors.As(err, &ue) {
				err = ue.Err
			}
			return fmt.Errorf("could not reach Backblaze: %v", err)
		}
		data, err = io.ReadAll(io.LimitReader(resp.Body, maxReply))
		resp.Body.Close()
		if err != nil {
			return errors.New("Backblaze's reply did not arrive whole")
		}
		if resp.StatusCode == http.StatusServiceUnavailable && attempt < maxUnavailableRetries {
			select {
			case <-req.Context().Done():
				return errors.New("could not reach Backblaze: the request was cancelled")
			case <-time.After(wait << attempt):
			}
			continue
		}
		if resp.StatusCode/100 != 2 {
			var e struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			}
			_ = json.Unmarshal(data, &e)
			msg := strings.ToLower(e.Message)
			return &APIError{Status: resp.StatusCode, Code: e.Code,
				noSuchKey: e.Code == "bad_request" && strings.Contains(msg, "key") && (strings.Contains(msg, "not exist") || strings.Contains(msg, "not found") || strings.Contains(msg, "no such"))}
		}
		break
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return errors.New("Backblaze's reply could not be read")
	}
	return nil
}

func (s *Session) call(ctx context.Context, name string, body, out any) error {
	raw, err := json.Marshal(body)
	if err != nil {
		return errors.New("could not build the request to Backblaze")
	}
	return s.client.send(func() (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.APIURL+"/b2api/v4/"+name, bytes.NewReader(raw))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", s.token)
		req.Header.Set("Content-Type", "application/json")
		return req, nil
	}, out)
}

// Revision is a bucket's revision. Backblaze's docs show it as an integer and its
// examples have used a string, so both read.
type Revision int64

// UnmarshalJSON accepts a number or a quoted number.
func (r *Revision) UnmarshalJSON(data []byte) error {
	text := strings.Trim(string(bytes.TrimSpace(data)), `"`)
	if text == "" || text == "null" {
		*r = 0
		return nil
	}
	v, err := strconv.ParseInt(text, 10, 64)
	if err != nil {
		return errors.New("a bucket revision that is not a number")
	}
	*r = Revision(v)
	return nil
}

// Bucket types.
const (
	BucketPrivate  = "allPrivate"
	BucketPublic   = "allPublic"
	BucketSnapshot = "snapshot"
)

// Bucket is one bucket as listed.
type Bucket struct {
	ID       string   `json:"bucketId"`
	Name     string   `json:"bucketName"`
	Type     string   `json:"bucketType"`
	Revision Revision `json:"revision"`
	Rules    Rules    `json:"lifecycleRules"`
}

// ListBuckets lists what the key can see: a key limited to buckets must name them.
func (s *Session) ListBuckets(ctx context.Context) ([]Bucket, error) {
	if len(s.Buckets) == 0 {
		return s.listBuckets(ctx, map[string]any{"accountId": s.AccountID, "bucketTypes": []string{BucketPrivate, BucketPublic, BucketSnapshot}})
	}
	out := []Bucket{}
	for _, ref := range s.Buckets {
		found, err := s.listBuckets(ctx, map[string]any{"accountId": s.AccountID, "bucketId": ref.ID})
		if err != nil {
			return nil, err
		}
		out = append(out, found...)
	}
	return out, nil
}

// FindBucket returns the named bucket, or nil when this key cannot see one.
func (s *Session) FindBucket(ctx context.Context, name string) (*Bucket, error) {
	if len(s.Buckets) > 0 {
		all, err := s.ListBuckets(ctx)
		if err != nil {
			return nil, err
		}
		for i := range all {
			if all[i].Name == name {
				return &all[i], nil
			}
		}
		return nil, nil
	}
	found, err := s.listBuckets(ctx, map[string]any{"accountId": s.AccountID, "bucketName": name})
	if err != nil {
		return nil, err
	}
	for i := range found {
		if found[i].Name == name {
			return &found[i], nil
		}
	}
	return nil, nil
}

func (s *Session) listBuckets(ctx context.Context, body map[string]any) ([]Bucket, error) {
	var reply struct {
		Buckets *[]Bucket `json:"buckets"`
	}
	if err := s.call(ctx, "b2_list_buckets", body, &reply); err != nil {
		return nil, err
	}
	if reply.Buckets == nil {
		return nil, errors.New("Backblaze's list of buckets could not be read")
	}
	for _, b := range *reply.Buckets {
		if b.ID == "" || b.Name == "" {
			return nil, errors.New("Backblaze's list of buckets could not be read")
		}
	}
	return *reply.Buckets, nil
}

// CreatePrivateBucket creates a private bucket.
func (s *Session) CreatePrivateBucket(ctx context.Context, name string) (*Bucket, error) {
	var b Bucket
	if err := s.call(ctx, "b2_create_bucket", map[string]any{"accountId": s.AccountID, "bucketName": name, "bucketType": BucketPrivate}, &b); err != nil {
		return nil, err
	}
	if b.ID == "" || b.Name != name {
		return nil, errors.New("Backblaze's reply to the new bucket could not be read")
	}
	return &b, nil
}

// UpdateRules replaces the bucket's rules if its revision has not moved.
func (s *Session) UpdateRules(ctx context.Context, bucketID string, rules Rules, revision Revision) error {
	if rules == nil {
		rules = Rules{}
	}
	var b Bucket
	err := s.call(ctx, "b2_update_bucket", map[string]any{"accountId": s.AccountID, "bucketId": bucketID, "lifecycleRules": rules, "ifRevisionIs": revision}, &b)
	if err != nil {
		return err
	}
	if b.ID != bucketID {
		return errors.New("Backblaze's reply to the bucket change could not be read")
	}
	return nil
}

// Key is a new application key. Secret exists only here: Backblaze shows it once, and
// the caller hands it to a server and drops it.
type Key struct {
	ID           string
	Name         string
	Secret       string
	Capabilities []string
}

// CreateKey makes a key limited to a bucket and a name prefix.
func (s *Session) CreateKey(ctx context.Context, name string, capabilities []string, bucketID, prefix string) (*Key, error) {
	var reply struct {
		ID           string   `json:"applicationKeyId"`
		Secret       string   `json:"applicationKey"`
		Capabilities []string `json:"capabilities"`
		KeyName      string   `json:"keyName"`
	}
	err := s.call(ctx, "b2_create_key", map[string]any{"accountId": s.AccountID, "capabilities": capabilities, "keyName": name,
		"bucketIds": []string{bucketID}, "namePrefix": prefix}, &reply)
	if err != nil {
		return nil, err
	}
	if reply.ID == "" || reply.Secret == "" {
		return nil, errors.New("Backblaze's reply to the new key could not be read")
	}
	return &Key{ID: reply.ID, Name: name, Secret: reply.Secret, Capabilities: reply.Capabilities}, nil
}

// DeleteKey deletes a key by its id.
func (s *Session) DeleteKey(ctx context.Context, keyID string) error {
	return s.call(ctx, "b2_delete_key", map[string]any{"applicationKeyId": keyID}, nil)
}

// ListFolders names what sits directly under prefix, up to max entries.
func (s *Session) ListFolders(ctx context.Context, bucketID, prefix string, max int) ([]string, error) {
	var reply struct {
		Files []struct {
			Name string `json:"fileName"`
		} `json:"files"`
	}
	err := s.call(ctx, "b2_list_file_names", map[string]any{"bucketId": bucketID, "prefix": prefix, "delimiter": "/", "maxFileCount": max}, &reply)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(reply.Files))
	for _, f := range reply.Files {
		out = append(out, f.Name)
	}
	return out, nil
}

// DeleteAllVersions removes every version under prefix and returns how many went. It
// stops at the first refusal, and what it already removed stays removed.
func (s *Session) DeleteAllVersions(ctx context.Context, bucketID, prefix string) (int, error) {
	if prefix == "" {
		return 0, errors.New("refusing to delete a whole bucket")
	}
	removed := 0
	startName, startID := "", ""
	for {
		body := map[string]any{"bucketId": bucketID, "prefix": prefix, "maxFileCount": 1000}
		if startName != "" {
			body["startFileName"] = startName
		}
		if startID != "" {
			body["startFileId"] = startID
		}
		var reply struct {
			Files []struct {
				Name string `json:"fileName"`
				ID   string `json:"fileId"`
			} `json:"files"`
			NextName *string `json:"nextFileName"`
			NextID   *string `json:"nextFileId"`
		}
		if err := s.call(ctx, "b2_list_file_versions", body, &reply); err != nil {
			return removed, err
		}
		for _, f := range reply.Files {
			if !strings.HasPrefix(f.Name, prefix) {
				return removed, errors.New("Backblaze listed a file outside the folder being removed, so removal stopped")
			}
			if err := s.call(ctx, "b2_delete_file_version", map[string]any{"fileName": f.Name, "fileId": f.ID}, nil); err != nil {
				return removed, err
			}
			removed++
		}
		if reply.NextName == nil || *reply.NextName == "" {
			return removed, nil
		}
		startName = *reply.NextName
		startID = ""
		if reply.NextID != nil {
			startID = *reply.NextID
		}
	}
}
