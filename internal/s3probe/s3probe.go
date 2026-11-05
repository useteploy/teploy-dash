// Package s3probe proves a set of S3-compatible credentials can write, read
// and delete one object, before Dash pushes them to a fleet. The CLI has no such
// command: `server backup-target set` only lists, and only warns when that fails.
//
// Standard library only. The probe uses path-style addressing, which R2, B2 and
// MinIO all accept. Nothing here logs, and errors carry the store's error code,
// never its message body: an InvalidAccessKeyId reply echoes the key.
package s3probe

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// Delete outcomes.
const (
	// DeleteRemoved: the store accepted the delete. On a store that keeps
	// versions that may only have hidden the object.
	DeleteRemoved = "removed"
	// DeleteRefused: the store said no (403). A B2 key made without deleteFiles
	// does this on purpose; the caller decides whether that is fine.
	DeleteRefused = "refused"
	// DeleteFailed: anything else (a 5xx, a dropped connection).
	DeleteFailed = "failed"
	// DeleteNotReached: an earlier step failed.
	DeleteNotReached = "not_reached"
)

// Config is one probe. Client is optional; give it a guarded transport when the
// endpoint came from a person.
type Config struct {
	Endpoint  string
	Region    string
	Bucket    string
	AccessKey string
	SecretKey string
	// Key is the object's full name under the bucket.
	Key    string
	Client *http.Client
	// Now pins the signing clock (tests).
	Now func() time.Time
}

// Result is what happened.
type Result struct {
	Write  bool   `json:"write"`
	Read   bool   `json:"read"`
	Delete string `json:"delete"`
	// Error names the first failed step in words, with the store's code.
	Error string `json:"error,omitempty"`
}

const maxBody = 1 << 16

// Run writes a small random object, reads it back and deletes it.
func Run(ctx context.Context, cfg Config) Result {
	res := Result{Delete: DeleteNotReached}
	if cfg.Client == nil {
		cfg.Client = &http.Client{Timeout: 30 * time.Second}
	}
	// Never follow a redirect with a signature, or towards another host.
	client := *cfg.Client
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	cfg.Client = &client

	payload := make([]byte, 32)
	if _, err := rand.Read(payload); err != nil {
		res.Error = "could not prepare the test object"
		return res
	}
	status, code, _, err := do(ctx, cfg, http.MethodPut, payload)
	if err != nil {
		res.Error = "write: " + err.Error()
		return res
	}
	if status/100 != 2 {
		res.Error = "write: " + describe(status, code)
		return res
	}
	res.Write = true

	status, code, body, err := do(ctx, cfg, http.MethodGet, nil)
	switch {
	case err != nil:
		res.Error = "read: " + err.Error()
	case status/100 != 2:
		res.Error = "read: " + describe(status, code)
	case !bytes.Equal(body, payload):
		res.Error = "read: the object came back different from what was written"
	default:
		res.Read = true
	}

	// The delete runs whatever the read said, so a failed read leaves nothing.
	status, code, _, err = do(ctx, cfg, http.MethodDelete, nil)
	switch {
	case err != nil:
		res.Delete = DeleteFailed
		if res.Error == "" {
			res.Error = "delete: " + err.Error()
		}
	case status/100 == 2:
		res.Delete = DeleteRemoved
	case status == http.StatusForbidden:
		res.Delete = DeleteRefused
		if res.Error == "" {
			res.Error = "delete: " + describe(status, code)
		}
	default:
		res.Delete = DeleteFailed
		if res.Error == "" {
			res.Error = "delete: " + describe(status, code)
		}
	}
	return res
}

func describe(status int, code string) string {
	if code != "" {
		return fmt.Sprintf("the store answered %d (%s)", status, code)
	}
	return fmt.Sprintf("the store answered %d", status)
}

func do(ctx context.Context, cfg Config, method string, body []byte) (status int, code string, reply []byte, err error) {
	target, err := objectURL(cfg.Endpoint, cfg.Bucket, cfg.Key)
	if err != nil {
		return 0, "", nil, err
	}
	req, err := http.NewRequestWithContext(ctx, method, target.String(), bytes.NewReader(body))
	if err != nil {
		return 0, "", nil, errors.New("could not build the request")
	}
	now := time.Now
	if cfg.Now != nil {
		now = cfg.Now
	}
	sign(req, body, cfg.AccessKey, cfg.SecretKey, cfg.Region, now().UTC())
	resp, err := cfg.Client.Do(req)
	if err != nil {
		// The dial guard's refusal is useful; a URL error's text is fine too (the
		// credentials are in headers, not the URL).
		return 0, "", nil, errors.New(scrubURLError(err))
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if resp.StatusCode/100 != 2 {
		var e struct {
			Code string `xml:"Code"`
		}
		if xml.Unmarshal(data, &e) == nil {
			code = e.Code
		}
		data = nil
	}
	return resp.StatusCode, code, data, nil
}

func scrubURLError(err error) string {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err.Error()
	}
	return err.Error()
}

// objectURL builds endpoint/bucket/key, each key segment escaped.
func objectURL(endpoint, bucket, key string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimRight(endpoint, "/"))
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, errors.New("the endpoint is not an http(s) URL")
	}
	u.Path = u.Path + "/" + bucket + "/" + key
	u.RawPath = ""
	return u, nil
}

// sign adds AWS Signature Version 4 headers, the S3 flavour: the canonical URI
// is encoded once and the payload hash is sent as a header.
func sign(req *http.Request, body []byte, accessKey, secretKey, region string, at time.Time) {
	amzDate := at.Format("20060102T150405Z")
	date := at.Format("20060102")
	sum := sha256.Sum256(body)
	payloadHash := hex.EncodeToString(sum[:])
	req.Header.Set("x-amz-date", amzDate)
	req.Header.Set("x-amz-content-sha256", payloadHash)
	host := req.URL.Host
	req.Host = host

	headers := map[string]string{"host": host}
	for name, values := range req.Header {
		headers[strings.ToLower(name)] = strings.TrimSpace(strings.Join(values, ","))
	}
	names := make([]string, 0, len(headers))
	for name := range headers {
		names = append(names, name)
	}
	sort.Strings(names)
	var canonicalHeaders strings.Builder
	for _, name := range names {
		canonicalHeaders.WriteString(name + ":" + headers[name] + "\n")
	}
	signedHeaders := strings.Join(names, ";")

	canonical := strings.Join([]string{
		req.Method,
		req.URL.EscapedPath(),
		req.URL.RawQuery,
		canonicalHeaders.String(),
		signedHeaders,
		payloadHash,
	}, "\n")
	scope := date + "/" + region + "/s3/aws4_request"
	canonicalSum := sha256.Sum256([]byte(canonical))
	toSign := strings.Join([]string{"AWS4-HMAC-SHA256", amzDate, scope, hex.EncodeToString(canonicalSum[:])}, "\n")

	key := hmacSHA256([]byte("AWS4"+secretKey), date)
	key = hmacSHA256(key, region)
	key = hmacSHA256(key, "s3")
	key = hmacSHA256(key, "aws4_request")
	signature := hex.EncodeToString(hmacSHA256(key, toSign))
	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential="+accessKey+"/"+scope+
		", SignedHeaders="+signedHeaders+", Signature="+signature)
}

func hmacSHA256(key []byte, data string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(data))
	return h.Sum(nil)
}
