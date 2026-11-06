package server

// backupstorage.go — "connect storage once" (plan P2.2). Probe from Dash, push
// `server backup-target set` to every server with the credentials on stdin, verify
// each server through `backup status`, and forget the credentials: they exist only
// on this handler's stack. Rotation is the same push with new keys, and the owner
// is told to revoke the old key only once every server has answered on the new one.

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/useteploy/teploy-dash/internal/monitor"
	"github.com/useteploy/teploy-dash/internal/s3probe"
)

// Storage providers.
const (
	backupProviderR2     = "r2"
	backupProviderB2     = "b2"
	backupProviderCustom = "custom"
)

const (
	backupPushConcurrency = 3
	// backupPushTimeout covers the CLI's set (which may install age and the aws
	// CLI, then rewrites cron lines) and the status read that verifies it.
	backupPushTimeout  = 10 * time.Minute
	backupProbeTimeout = 45 * time.Second
)

var (
	backupNameRE    = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]*$`)
	backupSegmentRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
	r2AccountRE     = regexp.MustCompile(`^[a-f0-9]{32}$`)
	b2RegionRE      = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,24}$`)
)

// backupStorage is what Dash remembers about the bucket: never a credential.
type backupStorage struct {
	Provider   string `json:"provider"`
	Bucket     string `json:"bucket"`
	Region     string `json:"region"`
	Endpoint   string `json:"endpoint,omitempty"`
	BasePrefix string `json:"base_prefix,omitempty"`
	// AllowInternal: the owner let this custom endpoint resolve to a private address.
	AllowInternal bool `json:"allow_internal,omitempty"`
	// Prefixes is each server's prefix: stable once assigned.
	Prefixes    map[string]string `json:"prefixes"`
	ConnectedAt time.Time         `json:"connected_at"`
	RotatedAt   *time.Time        `json:"rotated_at,omitempty"`
}

type backupStorageView struct {
	Provider    string            `json:"provider"`
	Bucket      string            `json:"bucket"`
	Region      string            `json:"region"`
	Endpoint    string            `json:"endpoint,omitempty"`
	BasePrefix  string            `json:"base_prefix,omitempty"`
	Prefixes    map[string]string `json:"prefixes"`
	ConnectedAt time.Time         `json:"connected_at"`
	RotatedAt   *time.Time        `json:"rotated_at,omitempty"`
}

func (r *backupRecord) storageViewLocked() *backupStorageView {
	if r.Storage == nil {
		return nil
	}
	st := r.Storage
	prefixes := make(map[string]string, len(st.Prefixes))
	for k, v := range st.Prefixes {
		prefixes[k] = v
	}
	return &backupStorageView{Provider: st.Provider, Bucket: st.Bucket, Region: st.Region, Endpoint: st.Endpoint,
		BasePrefix: st.BasePrefix, Prefixes: prefixes, ConnectedAt: st.ConnectedAt, RotatedAt: st.RotatedAt}
}

type backupStorageRequest struct {
	// Rotate pushes new keys to the connected bucket; only the keys are read.
	Rotate     bool   `json:"rotate"`
	Provider   string `json:"provider"`
	Bucket     string `json:"bucket"`
	Region     string `json:"region"`
	AccountID  string `json:"account_id"`
	Endpoint   string `json:"endpoint"`
	BasePrefix string `json:"base_prefix"`
	AccessKey  string `json:"access_key"`
	SecretKey  string `json:"secret_key"`
	// AllowInternal lets a custom endpoint resolve to a private address (an admin's
	// choice, as for monitors and channels).
	AllowInternal bool `json:"allow_internal"`
	// DeleteRefusalOK says a refused delete is deliberate on a non-B2 store (an R2
	// bucket lock).
	DeleteRefusalOK bool `json:"delete_refusal_ok"`
}

type backupPushResult struct {
	Server string `json:"server"`
	Prefix string `json:"prefix"`
	Pushed bool   `json:"pushed"`
	// Verified: the server stored this target and answered through it.
	Verified bool   `json:"verified"`
	Error    string `json:"error,omitempty"`
	// Replaced names a different target this push took over from.
	Replaced  string `json:"replaced,omitempty"`
	Lifecycle string `json:"incoming_lifecycle,omitempty"`
}

type backupStorageResponse struct {
	Probe      s3probe.Result     `json:"probe"`
	ProbeNote  string             `json:"probe_note,omitempty"`
	Servers    []backupPushResult `json:"servers"`
	Rotated    bool               `json:"rotated"`
	AllOK      bool               `json:"all_ok"`
	RevokeNote string             `json:"revoke_note,omitempty"`
}

func validSecretField(v string) bool {
	if v == "" || len(v) > 512 {
		return false
	}
	for _, c := range v {
		if c < 0x21 || c == 0x7f {
			return false
		}
	}
	return true
}

// resolveStorage fills region and endpoint from the preset.
func resolveStorage(body *backupStorageRequest) error {
	if !backupNameRE.MatchString(body.Bucket) || len(body.Bucket) > 63 {
		return fmt.Errorf("bucket name must be letters, digits, dots, dashes or underscores (at most 63)")
	}
	switch body.Provider {
	case backupProviderR2:
		if !r2AccountRE.MatchString(body.AccountID) {
			return fmt.Errorf("the Cloudflare account ID is 32 lowercase hex characters")
		}
		body.Region = "auto"
		body.Endpoint = "https://" + body.AccountID + ".r2.cloudflarestorage.com"
		body.AllowInternal = false
	case backupProviderB2:
		if !b2RegionRE.MatchString(body.Region) {
			return fmt.Errorf("the B2 region looks like us-west-004 (it is in the bucket's S3 endpoint)")
		}
		body.Endpoint = "https://s3." + body.Region + ".backblazeb2.com"
		body.AllowInternal = false
	case backupProviderCustom:
		u, err := url.Parse(body.Endpoint)
		if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil ||
			u.RawQuery != "" || u.Fragment != "" || strings.ContainsAny(body.Endpoint, " \t\r\n'\"\\") {
			return fmt.Errorf("the endpoint must be an http or https URL with no credentials, query or spaces")
		}
		if body.Region == "" {
			body.Region = "us-east-1"
		}
		if !backupNameRE.MatchString(body.Region) || len(body.Region) > 25 {
			return fmt.Errorf("the region must be letters, digits and dashes (at most 25)")
		}
	default:
		return fmt.Errorf("provider must be r2, b2 or custom")
	}
	body.BasePrefix = strings.Trim(body.BasePrefix, "/")
	if body.BasePrefix != "" {
		for _, seg := range strings.Split(body.BasePrefix, "/") {
			if !backupSegmentRE.MatchString(seg) {
				return fmt.Errorf("prefix segment %q must be letters, digits, dots, dashes or underscores", seg)
			}
		}
	}
	return nil
}

// backupServerPrefix is a server's prefix: always set, so a bucket can be shared
// and no two servers' namespaces collide.
func backupServerPrefix(base, server string, taken map[string]bool) string {
	slug := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			return r
		}
		return '-'
	}, server)
	slug = strings.TrimLeft(slug, "._-")
	if slug == "" {
		slug = "server"
	}
	join := func(seg string) string {
		if base == "" {
			return seg
		}
		return base + "/" + seg
	}
	prefix := join(slug)
	if taken[prefix] {
		h := fnv.New32a()
		h.Write([]byte(server))
		prefix = join(fmt.Sprintf("%s-%08x", slug, h.Sum32()))
	}
	return prefix
}

// scrubCredentials removes the pair from any text bound for a response or a log.
func scrubCredentials(text, accessKey, secretKey string) string {
	for _, v := range []string{accessKey, secretKey} {
		if v != "" {
			text = strings.ReplaceAll(text, v, "[redacted]")
		}
	}
	return sanitizeDomainError(text)
}

// handleBackupStorage serves POST /api/backups/storage.
func (s *Server) handleBackupStorage(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !s.requireBackupFleet(w, r) {
		return
	}
	var body backupStorageRequest
	if err := strictDecode(r, &body); err != nil {
		writeError(w, "invalid request body")
		return
	}
	if !validSecretField(body.AccessKey) || !validSecretField(body.SecretKey) {
		writeError(w, "enter both keys: each is one line of at most 512 characters with no spaces")
		return
	}
	b := &s.backups
	b.mu.Lock()
	current := b.rec.Storage
	b.mu.Unlock()
	if body.Rotate {
		if current == nil {
			writeError(w, "no storage is connected yet, so there is nothing to rotate")
			return
		}
		body.Provider, body.Bucket, body.Region, body.Endpoint, body.BasePrefix, body.AllowInternal =
			current.Provider, current.Bucket, current.Region, current.Endpoint, current.BasePrefix, current.AllowInternal
	} else if err := resolveStorage(&body); err != nil {
		writeError(w, err.Error())
		return
	}
	if body.AllowInternal && !s.isEffectiveAdmin(r) {
		writeErrorStatus(w, "reaching a private address needs an admin", http.StatusForbidden)
		return
	}
	servers, err := s.resolveServers(r.Context())
	if err != nil {
		writeErrorStatus(w, sanitizeDomainError("server discovery failed: "+err.Error()), http.StatusBadGateway)
		return
	}
	if len(servers) == 0 {
		writeError(w, "no servers to connect: add one in Fleet first")
		return
	}
	names := make([]string, 0, len(servers))
	for _, srv := range servers {
		names = append(names, srv.Name)
	}
	sort.Strings(names)

	prefixes := map[string]string{}
	taken := map[string]bool{}
	if current != nil && current.Bucket == body.Bucket && current.Endpoint == body.Endpoint {
		for k, v := range current.Prefixes {
			prefixes[k] = v
			taken[v] = true
		}
	}
	for _, name := range names {
		if prefixes[name] == "" {
			prefixes[name] = backupServerPrefix(body.BasePrefix, name, taken)
			taken[prefixes[name]] = true
		}
	}

	// A push and its verification finish once started: walking away from the page
	// must not leave a fleet half-rotated with nobody told.
	ctx := context.WithoutCancel(r.Context())
	probe, note, ok := s.probeBackupStorage(ctx, &body, prefixes[names[0]])
	resp := backupStorageResponse{Probe: probe, ProbeNote: note, Servers: []backupPushResult{}, Rotated: body.Rotate}
	if !ok {
		s.auditf(r, "backups.storage.probe_failed", body.Bucket, "provider=%s write=%v read=%v delete=%s", body.Provider, probe.Write, probe.Read, probe.Delete)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnprocessableEntity)
		json.NewEncoder(w).Encode(map[string]any{"error": "The storage probe failed, so nothing was pushed to any server. " + probe.Error, "data": resp})
		return
	}

	resp.Servers = s.pushBackupTarget(ctx, names, prefixes, &body)
	resp.AllOK = true
	pushedAny := false
	for _, res := range resp.Servers {
		resp.AllOK = resp.AllOK && res.Verified
		pushedAny = pushedAny || res.Pushed
	}
	if body.Rotate {
		if resp.AllOK {
			resp.RevokeNote = "Every server answered on the new key. You can now revoke the old key at your storage provider."
		} else {
			resp.RevokeNote = "Do not revoke the old key yet: not every server answered on the new one. Fix what is listed, then enter the new keys again."
		}
	}
	if pushedAny {
		now := time.Now().UTC()
		b.mu.Lock()
		if body.Rotate && b.rec.Storage != nil {
			b.rec.Storage.RotatedAt = &now
			b.rec.Storage.Prefixes = prefixes
		} else {
			b.rec.Storage = &backupStorage{Provider: body.Provider, Bucket: body.Bucket, Region: body.Region, Endpoint: body.Endpoint,
				BasePrefix: body.BasePrefix, AllowInternal: body.AllowInternal, Prefixes: prefixes, ConnectedAt: now}
		}
		b.saveLocked()
		b.mu.Unlock()
	}
	action := "backups.storage.connect"
	if body.Rotate {
		action = "backups.storage.rotate"
	}
	verified := 0
	for _, res := range resp.Servers {
		if res.Verified {
			verified++
		}
	}
	s.auditf(r, action, body.Bucket, "provider=%s servers=%d verified=%d delete=%s", body.Provider, len(resp.Servers), verified, probe.Delete)
	s.kickBackupSweep()
	writeData(w, resp)
}

// probeBackupStorage runs the write/read/delete probe. B2 keys deliberately lack
// deleteFiles (plan D2), so there a refused delete is the expected, safer setup.
// The object sits in the first server's .incoming/ tree, where the lifecycle rule
// the CLI's README asks for expires it if it could not be removed.
func (s *Server) probeBackupStorage(ctx context.Context, body *backupStorageRequest, firstPrefix string) (s3probe.Result, string, bool) {
	pctx, cancel := context.WithTimeout(ctx, backupProbeTimeout)
	defer cancel()
	client := &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{DialContext: monitor.PolicyDialContext(body.AllowInternal)}}
	res := s.backups.probe(pctx, s3probe.Config{
		Endpoint: body.Endpoint, Region: body.Region, Bucket: body.Bucket,
		AccessKey: body.AccessKey, SecretKey: body.SecretKey,
		Key:    firstPrefix + "/.incoming/dash-probe-" + randomHex(8),
		Client: client,
	})
	res.Error = scrubCredentials(res.Error, body.AccessKey, body.SecretKey)
	if !res.Write || !res.Read {
		return res, "", false
	}
	switch res.Delete {
	case s3probe.DeleteRemoved:
		res.Error = ""
		return res, "", true
	case s3probe.DeleteRefused:
		if body.Provider == backupProviderB2 {
			res.Error = ""
			return res, "This key cannot delete files, which is the recommended B2 setup: a compromised server cannot destroy history. Retention hides files, and the bucket's lifecycle rules remove them after 30 days. The test object stays under .incoming/ until that rule expires it.", true
		}
		if body.DeleteRefusalOK {
			res.Error = ""
			return res, "The store refused the delete, which you said is deliberate. Retention will report the objects it could not remove.", true
		}
		res.Error = "The store refused the delete, so retention could not remove old backups. If a bucket lock does this on purpose, tick the box and try again."
		return res, "", false
	}
	if res.Error == "" {
		res.Error = "the delete did not finish"
	}
	return res, "", false
}

func (s *Server) pushBackupTarget(ctx context.Context, names []string, prefixes map[string]string, body *backupStorageRequest) []backupPushResult {
	results := make([]backupPushResult, len(names))
	sem := make(chan struct{}, backupPushConcurrency)
	var wg sync.WaitGroup
	for i, name := range names {
		wg.Add(1)
		go func(i int, name string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			pctx, cancel := context.WithTimeout(ctx, backupPushTimeout)
			defer cancel()
			results[i] = s.pushBackupTargetOne(pctx, name, prefixes[name], body)
		}(i, name)
	}
	wg.Wait()
	return results
}

func (s *Server) pushBackupTargetOne(ctx context.Context, name, prefix string, body *backupStorageRequest) backupPushResult {
	res := backupPushResult{Server: name, Prefix: prefix}
	// What this push replaces is worth saying; a failed read here changes nothing.
	if st, err := s.readBackupTargetShow(ctx, name); err == nil && st.Configured && (st.Bucket != body.Bucket || st.Endpoint != body.Endpoint) {
		res.Replaced = "s3://" + st.Bucket
	}
	args := []string{"server", "backup-target", "set", "--host", name, "--bucket", body.Bucket, "--region", body.Region, "--prefix", prefix}
	if body.Endpoint != "" {
		args = append(args, "--endpoint", body.Endpoint)
	}
	if _, err := s.runCLIStdin(ctx, body.AccessKey+"\n"+body.SecretKey+"\n", args...); err != nil {
		res.Error = scrubCredentials(err.Error(), body.AccessKey, body.SecretKey)
		return res
	}
	res.Pushed = true
	env, err := s.readBackupStatus(ctx, name)
	s.recordBackupStatus(name, env, err, time.Now().UTC())
	if err != nil {
		res.Error = "stored, but could not be verified: " + scrubCredentials(err.Error(), body.AccessKey, body.SecretKey)
		return res
	}
	res.Lifecycle = env.Target.IncomingLifecycle
	switch {
	case !env.Target.Configured || env.Target.Bucket != body.Bucket || env.Target.Prefix != prefix:
		res.Error = "the server does not report the target that was just stored"
	case !env.Target.HasCredentials:
		res.Error = "the server stored the target without its credentials"
	case !env.Target.Reachable:
		res.Error = "the server cannot reach the bucket with these keys: " + scrubCredentials(env.Target.Error, body.AccessKey, body.SecretKey)
	default:
		res.Verified = true
	}
	return res
}

type backupTargetShow struct {
	Configured bool   `json:"configured"`
	Bucket     string `json:"bucket"`
	Endpoint   string `json:"endpoint"`
}

func (s *Server) readBackupTargetShow(ctx context.Context, name string) (*backupTargetShow, error) {
	args := []string{"server", "backup-target", "show", "--host", name, "--json"}
	result, err := s.runCLI(ctx, args...)
	if err != nil {
		return nil, err
	}
	if result.ExitCode != 0 {
		return nil, commandFailure(args, result)
	}
	var out backupTargetShow
	if err := json.Unmarshal([]byte(result.Stdout), &out); err != nil {
		return nil, fmt.Errorf("decoding backup target: %w", err)
	}
	return &out, nil
}

// backupLifecycleRule is the rule the CLI's README documents, scoped to this
// server's .incoming/ tree.
func backupLifecycleRule(st *backupStorage, target backupStatTarget) string {
	incoming := ".incoming/"
	root := ""
	if target.Prefix != "" {
		root = target.Prefix + "/"
		incoming = root + ".incoming/"
	}
	warn := " Scope it to that prefix: a rule on the whole bucket deletes the backups."
	if st != nil && st.Provider == backupProviderB2 || strings.Contains(target.Endpoint, "backblazeb2.com") {
		// B2 keeps hidden versions forever by default, so retention frees no storage
		// without the second rule. Its 30 days are also how long hidden backups can
		// still be recovered after a compromised server hides them. Overlapping rules
		// are allowed; B2 applies the smallest value of each property.
		return "Add two B2 lifecycle rules. First: fileNamePrefix " + incoming + ", daysFromUploadingToHiding 1, daysFromHidingToDeleting 1." + warn +
			" Second: fileNamePrefix " + root + ", daysFromHidingToDeleting 30, daysFromUploadingToHiding empty. Without it, backups that retention removes stay stored and billed forever; never set daysFromUploadingToHiding on this one."
	}
	return `aws s3api put-bucket-lifecycle-configuration --bucket ` + target.Bucket +
		` --lifecycle-configuration '{"Rules":[{"ID":"teploy-incoming","Status":"Enabled","Filter":{"Prefix":"` + incoming +
		`"},"Expiration":{"Days":1},"NoncurrentVersionExpiration":{"NoncurrentDays":1},"AbortIncompleteMultipartUpload":{"DaysAfterInitiation":1}}]}'` +
		" -" + warn
}
