package server

// backupsetup_test.go — "Protect my data" against a fake Backblaze and a stubbed CLI.
// Nothing here has met a real account: see the plan's real-account spike.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/useteploy/teploy-dash/internal/b2"
	"github.com/useteploy/teploy-dash/internal/caps"
	"github.com/useteploy/teploy-dash/internal/cli"
	"github.com/useteploy/teploy-dash/internal/s3probe"
)

const (
	testTempKeyID  = "TEMPKEYID0123"
	testTempKey    = "TEMPAPPKEYSECRET0123456789"
	testMintSecret = "MINTEDSECRET"
	testRecipient  = "age1lggyhqrw2nlhcxprm67z43rta597azn8gknawjehu9d9dl0jq3yqqvfafg"
)

// fakeBackblaze is the native API with the behaviours the failure rules turn on.
type fakeBackblaze struct {
	mu     sync.Mutex
	caps   []string
	expiry *int64
	// buckets by name.
	buckets  map[string]map[string]any
	revision int64
	// keys minted and still alive, by id.
	keys    map[string]map[string]any
	nextKey int
	deleted []string
	// refuseLifecycleCap makes create_key refuse readBucketLifecycleRules.
	refuseLifecycleCap bool
	// fail answers a call with this code, once per entry when failOnce.
	fail     map[string][2]any
	failOnce map[string]bool
	calls    []string
	bodies   map[string][]map[string]any
	echo     string
	url      string
}

func newFakeBackblaze(t *testing.T) (*fakeBackblaze, func() *b2.Client) {
	t.Helper()
	f := &fakeBackblaze{caps: []string{"listBuckets", "writeBuckets", "writeKeys", "deleteKeys"},
		buckets: map[string]map[string]any{}, keys: map[string]map[string]any{},
		fail: map[string][2]any{}, failOnce: map[string]bool{}, bodies: map[string][]map[string]any{}, revision: 1}
	exp := time.Now().Add(time.Hour).UnixMilli()
	f.expiry = &exp
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)
	f.url = srv.URL
	return f, func() *b2.Client { return &b2.Client{BaseURL: srv.URL, HTTP: srv.Client(), AnyHost: true} }
}

func (f *fakeBackblaze) addBucket(name, kind string, rules ...map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if rules == nil {
		rules = []map[string]any{}
	}
	f.buckets[name] = map[string]any{"bucketId": "id-" + name, "bucketName": name, "bucketType": kind, "revision": f.revision, "lifecycleRules": rules}
}

func (f *fakeBackblaze) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	name := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
	f.calls = append(f.calls, name)
	reply := func(status int, v any) {
		w.WriteHeader(status)
		json.NewEncoder(w).Encode(v)
	}
	if failure, ok := f.fail[name]; ok {
		if f.failOnce[name] {
			delete(f.fail, name)
		}
		reply(failure[0].(int), map[string]any{"status": failure[0], "code": failure[1], "message": "echo " + f.echo})
		return
	}
	var body map[string]any
	if r.Method == http.MethodPost {
		json.NewDecoder(r.Body).Decode(&body)
		f.bodies[name] = append(f.bodies[name], body)
	}
	switch name {
	case "b2_authorize_account":
		reply(200, map[string]any{"accountId": "acct", "authorizationToken": "TOKEN", "applicationKeyExpirationTimestamp": f.expiry,
			"apiInfo": map[string]any{"storageApi": map[string]any{"apiUrl": f.url, "s3ApiUrl": "https://s3.us-west-004.backblazeb2.com",
				"allowed": map[string]any{"capabilities": f.caps, "buckets": nil, "namePrefix": nil}}}})
	case "b2_list_buckets":
		out := []map[string]any{}
		for n, b := range f.buckets {
			if want, ok := body["bucketName"]; ok && want != n {
				continue
			}
			out = append(out, b)
		}
		reply(200, map[string]any{"buckets": out})
	case "b2_create_bucket":
		n := body["bucketName"].(string)
		if _, exists := f.buckets[n]; exists {
			reply(400, map[string]any{"code": "duplicate_bucket_name"})
			return
		}
		b := map[string]any{"bucketId": "id-" + n, "bucketName": n, "bucketType": body["bucketType"], "revision": f.revision, "lifecycleRules": []any{}}
		f.buckets[n] = b
		reply(200, b)
	case "b2_update_bucket":
		for _, b := range f.buckets {
			if b["bucketId"] == body["bucketId"] {
				if int64(body["ifRevisionIs"].(float64)) != b["revision"].(int64) {
					reply(409, map[string]any{"code": "conflict"})
					return
				}
				b["lifecycleRules"] = body["lifecycleRules"]
				b["revision"] = b["revision"].(int64) + 1
				reply(200, b)
				return
			}
		}
		reply(400, map[string]any{"code": "bad_bucket_id"})
	case "b2_create_key":
		capsList := body["capabilities"].([]any)
		if f.refuseLifecycleCap {
			for _, c := range capsList {
				if c == b2.CapReadLifecycle {
					reply(400, map[string]any{"code": "bad_request", "message": "unknown capability"})
					return
				}
			}
		}
		f.nextKey++
		id := fmt.Sprintf("MINTED%d", f.nextKey)
		f.keys[id] = body
		reply(200, map[string]any{"applicationKeyId": id, "applicationKey": testMintSecret + fmt.Sprint(f.nextKey), "capabilities": capsList, "keyName": body["keyName"]})
	case "b2_delete_key":
		id := body["applicationKeyId"].(string)
		f.deleted = append(f.deleted, id)
		delete(f.keys, id)
		reply(200, map[string]any{"applicationKeyId": id})
	case "b2_list_file_names":
		reply(200, map[string]any{"files": []any{}})
	case "b2_list_file_versions":
		reply(200, map[string]any{"files": []map[string]string{{"fileName": body["prefix"].(string) + "x", "fileId": "f1"}}})
	case "b2_delete_file_version":
		reply(200, map[string]any{})
	default:
		w.WriteHeader(500)
	}
}

func (f *fakeBackblaze) count(name string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if c == name {
			n++
		}
	}
	return n
}

func newSetupServer(t *testing.T) (*Server, *backupCLI, *fakeBackblaze) {
	t.Helper()
	c := &backupCLI{status: statusesFromTestdata(t), tokens: []string{"backup-setup-v1"},
		policyShow: map[string]string{"prod": `{"machine_interface":2,"configured":false}`, "stage": `{"machine_interface":2,"configured":false}`}}
	s := newBackupsServer(t, c)
	f, client := newFakeBackblaze(t)
	s.backups.b2Client = client
	s.backups.probe = func(context.Context, s3probe.Config) s3probe.Result {
		return s3probe.Result{Write: true, Read: true, Delete: s3probe.DeleteRemoved, Hidden: true}
	}
	return s, c, f
}

func applyBody(servers ...string) map[string]any {
	return map[string]any{"key_id": testTempKeyID, "application_key": testTempKey, "servers": servers}
}

func applySetup(t *testing.T, s *Server, body map[string]any) setupStorageResponse {
	t.Helper()
	rec := postJSON(t, s, "/api/backups/setup/storage/apply", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("apply: %d %s", rec.Code, rec.Body.String())
	}
	var resp setupStorageResponse
	decodeData(t, rec, &resp)
	return resp
}

func resultFor(t *testing.T, resp setupStorageResponse, server string) setupServerResult {
	t.Helper()
	for _, r := range resp.Servers {
		if r.Server == server {
			return r
		}
	}
	t.Fatalf("no result for %s: %+v", server, resp.Servers)
	return setupServerResult{}
}

// ── the feature gate and capability ──────────────────────────────────────────

func TestSetup_OlderCLIIsRefusedWithTheVersion(t *testing.T) {
	c := &backupCLI{status: statusesFromTestdata(t)} // no backup-setup-v1 token
	s := newBackupsServer(t, c)
	rec := postJSON(t, s, "/api/backups/setup/preflight", map[string]any{})
	if rec.Code != http.StatusNotImplemented || !strings.Contains(rec.Body.String(), backupSetupMinCLI) {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	if s.backupsView(context.Background(), backupsNow).Setup.Supported {
		t.Fatal("the page must not offer it")
	}
}

func TestSetup_EveryRouteNeedsAdministerCredentials(t *testing.T) {
	for _, sub := range []string{"preflight", "storage/check", "storage/apply", "owners", "recovery", "alerts/skip", "coverage", "turn-on", "run", "proof/start", "proof/result", "retire"} {
		got := requiredCapabilities(http.MethodPost, "/api/backups/setup/"+sub)
		if len(got) != 1 || got[0] != caps.AdministerCredentials {
			t.Errorf("%s = %v", sub, got)
		}
	}
	if got := requiredCapabilities(http.MethodGet, "/api/backups"); got[0] != caps.ViewMetadata {
		t.Errorf("the page itself stays readable: %v", got)
	}
}

func TestSetup_UnknownStepIsNotFound(t *testing.T) {
	s, _, _ := newSetupServer(t)
	if rec := postJSON(t, s, "/api/backups/setup/nope", map[string]any{}); rec.Code != http.StatusNotFound {
		t.Fatalf("%d", rec.Code)
	}
}

// ── preflight ────────────────────────────────────────────────────────────────

func corpusFixtureFile(t *testing.T, name string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "backup-contracts", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestSetupPreflight_SummarizesAndNeverTicksAConfiguredServer(t *testing.T) {
	s, c, _ := newSetupServer(t)
	c.hook = func(args []string, _ string) (*cli.Result, error, bool) {
		if !subsequence(args, []string{"server", "backup-preflight"}) {
			return nil, nil, false
		}
		switch args[3] {
		case "prod": // has a target
			return &cli.Result{Stdout: corpusFixtureFile(t, "preflight-ready.json")}, nil, true
		default: // needs everything
			return &cli.Result{Stdout: corpusFixtureFile(t, "preflight-needs-setup.json")}, nil, true
		}
	}
	rec := postJSON(t, s, "/api/backups/setup/preflight", map[string]any{})
	if rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Servers []backupPreflightSummary `json:"servers"`
	}
	decodeData(t, rec, &out)
	if len(out.Servers) != 2 || out.Servers[0].Server != "prod" {
		t.Fatalf("%+v", out.Servers)
	}
	prod, stage := out.Servers[0], out.Servers[1]
	if prod.Selected || prod.NeedsUpgrade || len(prod.NeedsTools) != 0 {
		t.Errorf("a server with a target starts unticked: %+v", prod)
	}
	if !stage.Selected || !stage.NeedsUpgrade || len(stage.NeedsTools) == 0 {
		t.Errorf("needs-setup: %+v", stage)
	}
	if n := c.countOf("--install-tools"); n != 0 {
		t.Fatalf("no tool install without consent: %d", n)
	}
}

func TestSetupPreflight_ConsentedActionsAndAFailedActionStillReports(t *testing.T) {
	s, c, _ := newSetupServer(t)
	c.hook = func(args []string, _ string) (*cli.Result, error, bool) {
		if !subsequence(args, []string{"server", "backup-preflight"}) {
			return nil, nil, false
		}
		// A flag's failure exits nonzero and still prints.
		return &cli.Result{Stdout: corpusFixtureFile(t, "preflight-after-actions.json"), ExitCode: 1}, fmt.Errorf("exit 1"), true
	}
	rec := postJSON(t, s, "/api/backups/setup/preflight", map[string]any{"servers": []string{"prod"}, "install_tools": []string{"prod"}, "upgrade_binary": []string{"prod"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Servers []backupPreflightSummary `json:"servers"`
	}
	decodeData(t, rec, &out)
	if len(out.Servers[0].Actions) == 0 || out.Servers[0].Blocked != "" {
		t.Fatalf("the report wins: %+v", out.Servers[0])
	}
	if c.countOf("backup-preflight", "--install-tools", "--upgrade-binary") != 1 {
		t.Fatal("the consented flags must reach the CLI")
	}
}

func TestSetupPreflight_UnreachableAndUnreadable(t *testing.T) {
	s, c, _ := newSetupServer(t)
	c.hook = func(args []string, _ string) (*cli.Result, error, bool) {
		if !subsequence(args, []string{"server", "backup-preflight"}) {
			return nil, nil, false
		}
		if args[3] == "prod" {
			return &cli.Result{Stdout: corpusFixtureFile(t, "preflight-unreachable.json")}, nil, true
		}
		return &cli.Result{Stderr: "boom", ExitCode: 1}, nil, true
	}
	rec := postJSON(t, s, "/api/backups/setup/preflight", map[string]any{})
	var out struct {
		Servers []backupPreflightSummary `json:"servers"`
	}
	decodeData(t, rec, &out)
	for _, sum := range out.Servers {
		if sum.Blocked == "" || sum.Selected {
			t.Errorf("%s must be blocked and unticked: %+v", sum.Server, sum)
		}
	}
}

func TestSetupPreflight_UnknownNeverReadsFine(t *testing.T) {
	p, err := decodeBackupPreflight(corpusFixtureFile(t, "preflight-unknown.json"))
	if err != nil {
		t.Fatal(err)
	}
	sum := summarizeBackupPreflight("x", p)
	if sum.Selected || len(sum.Warnings) == 0 {
		t.Fatalf("an unread target starts unticked and says so: %+v", sum)
	}
}

// ── storage: check ───────────────────────────────────────────────────────────

func TestSetupCheck_ReportsExactMissingCapabilities(t *testing.T) {
	s, _, f := newSetupServer(t)
	f.caps = []string{"listBuckets"}
	rec := postJSON(t, s, "/api/backups/setup/storage/check", map[string]any{"key_id": testTempKeyID, "application_key": testTempKey})
	var out map[string]any
	decodeData(t, rec, &out)
	if fmt.Sprint(out["missing"]) != "[writeBuckets writeKeys]" {
		t.Fatalf("%+v", out)
	}
}

func TestSetupCheck_WarnsWhenTheKeyNeverExpires(t *testing.T) {
	s, _, f := newSetupServer(t)
	f.expiry = nil
	rec := postJSON(t, s, "/api/backups/setup/storage/check", map[string]any{"key_id": testTempKeyID, "application_key": testTempKey})
	var out struct {
		NeverExpires bool     `json:"never_expires"`
		Warnings     []string `json:"warnings"`
	}
	decodeData(t, rec, &out)
	if !out.NeverExpires || !strings.Contains(strings.Join(out.Warnings, " "), "never expires") {
		t.Fatalf("%+v", out)
	}
}

func TestSetupCheck_ACappedListingIsNotUnreachable(t *testing.T) {
	s, _, f := newSetupServer(t)
	f.fail["b2_list_buckets"] = [2]any{403, "transaction_cap_exceeded"}
	rec := postJSON(t, s, "/api/backups/setup/storage/check", map[string]any{"key_id": testTempKeyID, "application_key": testTempKey})
	if rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Capped       bool   `json:"capped"`
		ListingError string `json:"listing_error"`
	}
	decodeData(t, rec, &out)
	if !out.Capped || !strings.Contains(out.ListingError, "daily transaction cap") || strings.Contains(strings.ToLower(out.ListingError), "unreachable") {
		t.Fatalf("%+v", out)
	}
}

func TestSetupCheck_RefusesAndWarnsPerBucket(t *testing.T) {
	s, _, f := newSetupServer(t)
	f.addBucket("bad", "allPrivate", map[string]any{"fileNamePrefix": "", "daysFromUploadingToHiding": 30})
	f.addBucket("short", "allPrivate", map[string]any{"fileNamePrefix": "teploy/", "daysFromHidingToDeleting": 2})
	f.addBucket("pub", "allPublic")
	f.addBucket("fine", "allPrivate")
	rec := postJSON(t, s, "/api/backups/setup/storage/check", map[string]any{"key_id": testTempKeyID, "application_key": testTempKey})
	var out struct {
		Buckets []backupBucketFinding `json:"buckets"`
	}
	decodeData(t, rec, &out)
	got := map[string]backupBucketFinding{}
	for _, b := range out.Buckets {
		got[b.Name] = b
	}
	if len(got["bad"].Refusals) == 0 || len(got["pub"].Refusals) == 0 || len(got["short"].Warnings) == 0 || len(got["short"].Refusals) != 0 || len(got["fine"].Refusals)+len(got["fine"].Warnings) != 0 {
		t.Fatalf("%+v", got)
	}
}

// ── storage: apply ───────────────────────────────────────────────────────────

func TestSetupApply_HappyPath(t *testing.T) {
	s, c, f := newSetupServer(t)
	resp := applySetup(t, s, applyBody("prod", "stage"))
	if !resp.AllOK || !resp.BucketCreated || resp.Region != "us-west-004" || resp.Base != "teploy" || resp.RulesState != "set" {
		t.Fatalf("%+v", resp)
	}
	if !strings.HasPrefix(resp.Bucket, "teploy-backups-") {
		t.Fatalf("bucket = %s", resp.Bucket)
	}
	if f.bodies["b2_create_bucket"][0]["bucketType"] != "allPrivate" {
		t.Fatal("a created bucket is private")
	}
	// One limited key per server, without deleteFiles, with a clean name.
	if len(f.keys) != 2 {
		t.Fatalf("keys = %+v", f.keys)
	}
	seen := map[string]bool{}
	for _, body := range f.keys {
		capsList := fmt.Sprint(body["capabilities"])
		if strings.Contains(capsList, "deleteFiles") {
			t.Fatalf("a server key must not have deleteFiles: %s", capsList)
		}
		seen[body["namePrefix"].(string)] = true
		for _, r := range body["keyName"].(string) {
			if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-') {
				t.Fatalf("key name %q", body["keyName"])
			}
		}
		if ids := body["bucketIds"].([]any); len(ids) != 1 {
			t.Fatalf("limited to the bucket: %+v", body)
		}
	}
	if !seen["teploy/prod/"] || !seen["teploy/stage/"] {
		t.Fatalf("prefixes = %+v", seen)
	}
	// The rules.
	rules := f.buckets[resp.Bucket]["lifecycleRules"]
	raw, _ := json.Marshal(rules)
	for _, want := range []string{`"daysFromHidingToDeleting":14,"daysFromStartingToCancelingUnfinishedLargeFiles":7,"daysFromUploadingToHiding":null,"fileNamePrefix":"teploy/"`, `"fileNamePrefix":"teploy/prod/.incoming/"`, `"fileNamePrefix":"teploy/stage/.incoming/"`} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("rules %s miss %s", raw, want)
		}
	}
	// Each secret rode stdin.
	if n := c.countOf("backup-target", "set"); n != 2 {
		t.Fatalf("pushes = %d", n)
	}
	c.mu.Lock()
	for _, call := range c.calls {
		if subsequence(call.args, []string{"backup-target", "set"}) {
			if !strings.Contains(call.stdin, testMintSecret) {
				t.Errorf("secret must ride stdin: %q", call.stdin)
			}
			if strings.Contains(strings.Join(call.args, " "), testMintSecret) {
				t.Errorf("secret in argv")
			}
		}
	}
	c.mu.Unlock()
	// The temporary key has an expiry and deleteKeys, so it goes.
	if fmt.Sprint(f.deleted) != "["+testTempKeyID+"]" || !strings.Contains(resp.KeyNote, "deleted your temporary key") {
		t.Fatalf("deleted=%v note=%q", f.deleted, resp.KeyNote)
	}
	// Dash remembers ids and names only.
	s.backups.mu.Lock()
	st := s.backups.rec.Storage
	k := s.backups.rec.Setup.Keys["prod"]
	s.backups.mu.Unlock()
	if st.Provider != "b2" || st.Prefixes["prod"] != "teploy/prod" || k.State != backupKeyVerified || k.Prefix != "teploy/prod" {
		t.Fatalf("storage=%+v key=%+v", st, k)
	}
}

func TestSetupApply_MissingCapabilityStopsEarly(t *testing.T) {
	s, c, f := newSetupServer(t)
	f.caps = []string{"listBuckets", "writeBuckets"}
	rec := postJSON(t, s, "/api/backups/setup/storage/apply", applyBody("prod"))
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "writeKeys") {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	if f.count("b2_create_bucket")+f.count("b2_create_key") != 0 || c.countOf("backup-target", "set") != 0 {
		t.Fatal("nothing may happen on a key that cannot finish")
	}
}

func TestSetupApply_NoServersTicked(t *testing.T) {
	s, _, _ := newSetupServer(t)
	if rec := postJSON(t, s, "/api/backups/setup/storage/apply", applyBody()); rec.Code != http.StatusBadRequest {
		t.Fatalf("%d", rec.Code)
	}
	if rec := postJSON(t, s, "/api/backups/setup/storage/apply", applyBody("nosuch")); rec.Code != http.StatusBadRequest {
		t.Fatalf("%d", rec.Code)
	}
}

func TestSetupApply_AnUntickedServerIsNeverTouched(t *testing.T) {
	s, c, _ := newSetupServer(t)
	resp := applySetup(t, s, applyBody("prod"))
	if !resp.AllOK {
		t.Fatalf("%+v", resp)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, call := range c.calls {
		if strings.Contains(strings.Join(call.args, " "), "stage") {
			t.Fatalf("an unticked server must not be touched: %v", call.args)
		}
	}
}

func TestSetupApply_RetryNeverMakesASecondBucket(t *testing.T) {
	s, _, f := newSetupServer(t)
	// The create fails after the name was recorded.
	f.fail["b2_create_bucket"] = [2]any{500, "internal_error"}
	f.failOnce["b2_create_bucket"] = true
	if rec := postJSON(t, s, "/api/backups/setup/storage/apply", applyBody("prod")); rec.Code == http.StatusOK {
		t.Fatalf("%s", rec.Body.String())
	}
	s.backups.mu.Lock()
	recorded := s.backups.rec.Setup.Bucket
	s.backups.mu.Unlock()
	if recorded == "" {
		t.Fatal("the name must be recorded before the create")
	}
	resp := applySetup(t, s, applyBody("prod"))
	if resp.Bucket != recorded || len(f.buckets) != 1 {
		t.Fatalf("retry made another: %s, %+v", resp.Bucket, f.buckets)
	}
	// And a create that landed but whose reply was lost finds the bucket, not a new one.
	s2, _, f2 := newSetupServer(t)
	s2.backups.mu.Lock()
	s2.backups.setupLocked().Bucket = "teploy-backups-landed"
	s2.backups.mu.Unlock()
	f2.addBucket("teploy-backups-landed", "allPrivate")
	resp = applySetup(t, s2, applyBody("prod"))
	if resp.Bucket != "teploy-backups-landed" || resp.BucketCreated || f2.count("b2_create_bucket") != 0 {
		t.Fatalf("%+v", resp)
	}
}

func TestSetupApply_ANameTakenByAnotherAccountChoosesAgain(t *testing.T) {
	s, _, f := newSetupServer(t)
	s.backups.mu.Lock()
	s.backups.setupLocked().Bucket = "teploy-backups-taken"
	s.backups.mu.Unlock()
	f.fail["b2_create_bucket"] = [2]any{400, "duplicate_bucket_name"}
	f.failOnce["b2_create_bucket"] = true
	resp := applySetup(t, s, applyBody("prod"))
	if resp.Bucket == "teploy-backups-taken" || !strings.HasPrefix(resp.Bucket, "teploy-backups-") {
		t.Fatalf("%s", resp.Bucket)
	}
}

func TestSetupApply_RerunSkipsVerifiedServers(t *testing.T) {
	s, c, f := newSetupServer(t)
	f.expiry = nil // keep the temp key usable across runs
	first := applySetup(t, s, applyBody("prod", "stage"))
	minted := f.nextKey
	second := applySetup(t, s, applyBody("prod", "stage"))
	if second.Bucket != first.Bucket || f.nextKey != minted {
		t.Fatalf("a rerun must mint nothing: %d -> %d", minted, f.nextKey)
	}
	if !second.AllOK || !strings.Contains(resultFor(t, second, "prod").Skipped, "verified") {
		t.Fatalf("%+v", second)
	}
	if n := c.countOf("backup-target", "set"); n != 2 {
		t.Fatalf("a rerun must push nothing: %d", n)
	}
}

func TestSetupApply_PushFailedAndProvablyMissed_DeletesTheKey(t *testing.T) {
	s, c, f := newSetupServer(t)
	c.hook = func(args []string, _ string) (*cli.Result, error, bool) {
		if subsequence(args, []string{"backup-target", "set"}) && subsequence(args, []string{"stage"}) {
			return &cli.Result{ExitCode: 1, Stderr: "no route"}, fmt.Errorf("exit 1: no route"), true
		}
		if subsequence(args, []string{"backup-target", "show"}) && subsequence(args, []string{"stage"}) {
			return &cli.Result{Stdout: `{"machine_interface":2,"configured":false}`}, nil, true
		}
		return nil, nil, false
	}
	resp := applySetup(t, s, applyBody("prod", "stage"))
	stage := resultFor(t, resp, "stage")
	if resp.AllOK || stage.Pushed || stage.KeyKept || !stage.KeyDeleted {
		t.Fatalf("%+v", stage)
	}
	if strings.Contains(fmt.Sprint(f.deleted), testTempKeyID) {
		t.Fatal("a partial run keeps the temporary key")
	}
	if !strings.Contains(resp.KeyNote, "paste it again") {
		t.Fatalf("note = %q", resp.KeyNote)
	}
	s.backups.mu.Lock()
	_, kept := s.backups.rec.Setup.Keys["stage"]
	_, prodKept := s.backups.rec.Setup.Keys["prod"]
	s.backups.mu.Unlock()
	if kept || !prodKept {
		t.Fatalf("records: stage=%v prod=%v", kept, prodKept)
	}
}

func TestSetupApply_PushFailedButMayHaveLanded_KeepsTheKey(t *testing.T) {
	for name, show := range map[string]func() (*cli.Result, error){
		"server stopped answering": func() (*cli.Result, error) { return nil, fmt.Errorf("exit 1: timeout") },
		"it stores this target": func() (*cli.Result, error) {
			return &cli.Result{Stdout: `{"configured":true,"bucket":"PLACEHOLDER","prefix":"teploy/stage"}`}, nil
		},
	} {
		t.Run(name, func(t *testing.T) {
			s, c, f := newSetupServer(t)
			f.addBucket("chosen", "allPrivate")
			c.hook = func(args []string, _ string) (*cli.Result, error, bool) {
				if subsequence(args, []string{"backup-target", "set"}) && subsequence(args, []string{"stage"}) {
					return &cli.Result{ExitCode: 1, Stderr: "cron rewrite failed"}, fmt.Errorf("exit 1"), true
				}
				if subsequence(args, []string{"backup-target", "show"}) && subsequence(args, []string{"stage"}) {
					res, err := show()
					if res != nil {
						res.Stdout = strings.ReplaceAll(res.Stdout, "PLACEHOLDER", "chosen")
					}
					return res, err, true
				}
				return nil, nil, false
			}
			body := applyBody("stage")
			body["bucket"] = "chosen"
			resp := applySetup(t, s, body)
			stage := resultFor(t, resp, "stage")
			if !stage.KeyKept || stage.KeyDeleted || len(f.deleted) != 0 {
				t.Fatalf("%+v deleted=%v", stage, f.deleted)
			}
		})
	}
}

func TestSetupApply_PushedButUnverifiedKeepsTheKeyThenRerunRecovers(t *testing.T) {
	s, c, f := newSetupServer(t)
	f.expiry = nil
	bad := true
	orig := c.status["stage"]
	c.status["stage"] = func() (*cli.Result, error) {
		if bad {
			return nil, fmt.Errorf("exit 1: timeout")
		}
		return orig()
	}
	resp := applySetup(t, s, applyBody("stage"))
	stage := resultFor(t, resp, "stage")
	if !stage.Pushed || stage.Verified || !stage.KeyKept || len(f.keys) != 1 {
		t.Fatalf("%+v keys=%v", stage, f.keys)
	}
	if strings.Contains(fmt.Sprint(f.deleted), "MINTED") {
		t.Fatalf("a pushed key must stay: %v", f.deleted)
	}
	// Next try the server answers on the key it already has: nothing new is minted.
	bad = false
	minted := f.nextKey
	resp = applySetup(t, s, applyBody("stage"))
	stage = resultFor(t, resp, "stage")
	if !stage.Verified || f.nextKey != minted || !resp.AllOK {
		t.Fatalf("%+v minted %d->%d", stage, minted, f.nextKey)
	}
}

func TestSetupApply_PushedUnverifiedRerunReplacesThenDeletesTheOldKey(t *testing.T) {
	s, c, f := newSetupServer(t)
	f.expiry = nil
	failing := true
	orig := c.status["stage"]
	c.status["stage"] = func() (*cli.Result, error) {
		if failing {
			return nil, fmt.Errorf("exit 1: timeout")
		}
		return orig()
	}
	applySetup(t, s, applyBody("stage"))
	s.backups.mu.Lock()
	oldID := s.backups.rec.Setup.Keys["stage"].KeyID
	s.backups.mu.Unlock()
	// The re-verify fails too, so a new key is made, and verification succeeds on it.
	calls := 0
	c.status["stage"] = func() (*cli.Result, error) {
		calls++
		if calls == 1 {
			return nil, fmt.Errorf("exit 1: timeout")
		}
		return orig()
	}
	resp := applySetup(t, s, applyBody("stage"))
	if !resultFor(t, resp, "stage").Verified || !strings.Contains(fmt.Sprint(f.deleted), oldID) {
		t.Fatalf("%+v deleted=%v", resp, f.deleted)
	}
}

func TestSetupApply_RotationOrderIsMintPushVerifyDeleteOld(t *testing.T) {
	s, c, f := newSetupServer(t)
	f.expiry = nil
	applySetup(t, s, applyBody("prod"))
	s.backups.mu.Lock()
	oldID := s.backups.rec.Setup.Keys["prod"].KeyID
	s.backups.mu.Unlock()

	// Failed verification: the old key stays.
	orig := c.status["prod"]
	c.status["prod"] = func() (*cli.Result, error) { return nil, fmt.Errorf("exit 1: timeout") }
	body := applyBody("prod")
	body["rotate"] = true
	resp := applySetup(t, s, body)
	if resultFor(t, resp, "prod").Verified || strings.Contains(fmt.Sprint(f.deleted), oldID) {
		t.Fatalf("an unverified rotation must keep the old key: %v", f.deleted)
	}
	// Verified: the new one is in place and the replaced key goes.
	c.status["prod"] = orig
	s.backups.mu.Lock()
	newID := s.backups.rec.Setup.Keys["prod"].KeyID
	s.backups.mu.Unlock()
	resp = applySetup(t, s, body)
	if !resultFor(t, resp, "prod").Verified || !strings.Contains(fmt.Sprint(f.deleted), newID) || !strings.Contains(fmt.Sprint(f.deleted), oldID) {
		t.Fatalf("%+v deleted=%v", resp, f.deleted)
	}
}

func TestSetupStorage_AManualKeyReportsTheOneItReplaced(t *testing.T) {
	s, _, f := newSetupServer(t)
	f.expiry = nil
	applySetup(t, s, applyBody("prod"))
	s.backups.mu.Lock()
	bucket := s.backups.rec.Storage.Bucket
	s.backups.mu.Unlock()
	body := storageBody()
	body["bucket"] = bucket
	if rec := postJSON(t, s, "/api/backups/storage", body); rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	view := s.backupsView(context.Background(), backupsNow)
	if len(view.Setup.Leftovers) != 1 || !strings.Contains(view.Setup.Leftovers[0].Reason, "by hand") {
		t.Fatalf("%+v", view.Setup.Leftovers)
	}
}

func TestSetupApply_RefusesAConflictingRuleBeforeAnyKey(t *testing.T) {
	s, c, f := newSetupServer(t)
	f.addBucket("old", "allPrivate", map[string]any{"fileNamePrefix": "teploy/", "daysFromUploadingToHiding": 30, "daysFromHidingToDeleting": 1})
	body := applyBody("prod")
	body["bucket"] = "old"
	body["accept_warnings"] = true
	rec := postJSON(t, s, "/api/backups/setup/storage/apply", body)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "hides files") {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	if f.count("b2_create_key")+f.count("b2_update_bucket") != 0 || c.countOf("backup-target", "set") != 0 {
		t.Fatal("a refused bucket gets nothing")
	}
}

func TestSetupApply_APublicBucketIsRefused(t *testing.T) {
	s, _, f := newSetupServer(t)
	f.addBucket("pub", "allPublic")
	body := applyBody("prod")
	body["bucket"] = "pub"
	if rec := postJSON(t, s, "/api/backups/setup/storage/apply", body); rec.Code != http.StatusConflict {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
}

func TestSetupApply_WarningsNeedConfirmation(t *testing.T) {
	s, _, f := newSetupServer(t)
	f.addBucket("keep-last", "allPrivate", map[string]any{"fileNamePrefix": "", "daysFromHidingToDeleting": 1})
	body := applyBody("prod")
	body["bucket"] = "keep-last"
	rec := postJSON(t, s, "/api/backups/setup/storage/apply", body)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "keeps only the last version") {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	if f.count("b2_create_key") != 0 {
		t.Fatal("no key before the warning is read")
	}
	body["accept_warnings"] = true
	if !applySetup(t, s, body).AllOK {
		t.Fatal("confirmed runs")
	}
}

func TestSetupApply_MergesWithRevisionAndKeepsOtherRules(t *testing.T) {
	s, _, f := newSetupServer(t)
	f.addBucket("shared", "allPrivate", map[string]any{"fileNamePrefix": "photos/", "daysFromHidingToDeleting": 9})
	// Someone edits the bucket at the same moment: one conflict, then a clean retry.
	f.fail["b2_update_bucket"] = [2]any{409, "conflict"}
	f.failOnce["b2_update_bucket"] = true
	body := applyBody("prod")
	body["bucket"] = "shared"
	resp := applySetup(t, s, body)
	if !resp.AllOK || resp.RulesState != "set" {
		t.Fatalf("%+v", resp)
	}
	raw, _ := json.Marshal(f.buckets["shared"]["lifecycleRules"])
	if !strings.Contains(string(raw), `"photos/"`) || !strings.Contains(string(raw), `"teploy/"`) {
		t.Fatalf("rules = %s", raw)
	}
	for _, b := range f.bodies["b2_update_bucket"] {
		if _, ok := b["ifRevisionIs"]; !ok {
			t.Fatal("every update must carry ifRevisionIs")
		}
	}
}

func TestSetupApply_NeverSendsLifecycleRulesToAnS3ManagedBucket(t *testing.T) {
	s, _, f := newSetupServer(t)
	// Detected: a rule with a field the native API does not document.
	f.addBucket("s3ish", "allPrivate", map[string]any{"fileNamePrefix": "x/", "id": "rule-1"})
	body := applyBody("prod")
	body["bucket"] = "s3ish"
	resp := applySetup(t, s, body)
	if resp.RulesState != "s3_managed" || f.count("b2_update_bucket") != 0 || len(resp.RulesText) == 0 {
		t.Fatalf("%+v updates=%d", resp, f.count("b2_update_bucket"))
	}
	if !strings.Contains(strings.Join(resp.Warnings, " "), "no sure sign") {
		t.Fatalf("it must say the detection is a best effort: %v", resp.Warnings)
	}
	// Declared: the person's tick.
	s2, _, f2 := newSetupServer(t)
	f2.addBucket("mine", "allPrivate")
	body = applyBody("prod")
	body["bucket"], body["s3_managed"] = "mine", true
	if resp := applySetup(t, s2, body); resp.RulesState != "s3_managed" || f2.count("b2_update_bucket") != 0 {
		t.Fatalf("%+v", resp)
	}
}

func TestSetupApply_LifecycleCapabilityRefusedRetriesWithout(t *testing.T) {
	s, _, f := newSetupServer(t)
	f.refuseLifecycleCap = true
	resp := applySetup(t, s, applyBody("prod"))
	prod := resultFor(t, resp, "prod")
	if !prod.Verified || prod.Lifecycle != "unknown" || !strings.Contains(prod.Note, "lifecycle") {
		t.Fatalf("%+v", prod)
	}
	if f.count("b2_create_key") != 2 {
		t.Fatalf("create_key = %d", f.count("b2_create_key"))
	}
	s.backups.mu.Lock()
	defer s.backups.mu.Unlock()
	if s.backups.rec.Setup.Keys["prod"].Lifecycle != "unknown" {
		t.Fatal("the record must say so too")
	}
}

func TestSetupApply_HideRefusalStopsEverythingAndDeletesTheKey(t *testing.T) {
	for name, probe := range map[string]s3probe.Result{
		"refused":       {Write: true, Read: true, Delete: s3probe.DeleteRefused, Error: "delete: the store answered 403 (AccessDenied)"},
		"still-visible": {Write: true, Read: true, Delete: s3probe.DeleteRemoved, Hidden: false},
	} {
		t.Run(name, func(t *testing.T) {
			s, c, f := newSetupServer(t)
			s.backups.probe = func(context.Context, s3probe.Config) s3probe.Result { return probe }
			resp := applySetup(t, s, applyBody("prod", "stage"))
			if resp.AllOK || !strings.Contains(strings.ToLower(resp.Stopped), "retention") {
				t.Fatalf("%+v", resp)
			}
			if n := c.countOf("backup-target", "set"); n != 0 {
				t.Fatalf("nothing may be pushed: %d", n)
			}
			if len(f.keys) != 0 {
				t.Fatalf("a key never pushed must be deleted: %+v", f.keys)
			}
			if strings.Contains(fmt.Sprint(f.deleted), testTempKeyID) {
				t.Fatal("the temporary key stays for the retry")
			}
			s.backups.mu.Lock()
			defer s.backups.mu.Unlock()
			if s.backups.rec.Storage != nil || len(s.backups.rec.Setup.Keys) != 0 {
				t.Fatalf("nothing is recorded: %+v", s.backups.rec.Setup.Keys)
			}
		})
	}
}

func TestSetupApply_ABadKeyDeletesOnlyThatKey(t *testing.T) {
	s, c, f := newSetupServer(t)
	s.backups.probe = func(_ context.Context, cfg s3probe.Config) s3probe.Result {
		if strings.Contains(cfg.Key, "stage") {
			return s3probe.Result{Delete: s3probe.DeleteNotReached, Error: "write: the store answered 403"}
		}
		return s3probe.Result{Write: true, Read: true, Delete: s3probe.DeleteRemoved, Hidden: true}
	}
	resp := applySetup(t, s, applyBody("prod", "stage"))
	if resp.Stopped != "" || !resultFor(t, resp, "prod").Verified || !resultFor(t, resp, "stage").KeyDeleted {
		t.Fatalf("%+v", resp)
	}
	if n := c.countOf("backup-target", "set"); n != 1 {
		t.Fatalf("pushes = %d", n)
	}
	if len(f.keys) != 1 {
		t.Fatalf("keys = %+v", f.keys)
	}
}

func TestSetupApply_WithoutDeleteKeysAStrandedKeyIsReported(t *testing.T) {
	s, _, f := newSetupServer(t)
	f.caps = []string{"listBuckets", "writeBuckets", "writeKeys"}
	s.backups.probe = func(context.Context, s3probe.Config) s3probe.Result {
		return s3probe.Result{Delete: s3probe.DeleteNotReached, Error: "write: the store answered 403"}
	}
	resp := applySetup(t, s, applyBody("prod"))
	if resultFor(t, resp, "prod").KeyDeleted {
		t.Fatal("this key cannot delete keys")
	}
	view := s.backupsView(context.Background(), backupsNow)
	if len(view.Setup.Leftovers) != 1 || view.Setup.Leftovers[0].Server != "prod" {
		t.Fatalf("leftovers = %+v", view.Setup.Leftovers)
	}
}

func TestSetupApply_CapErrorsArePlain(t *testing.T) {
	cases := map[string]struct {
		call, code, want string
		status           int
	}{
		"cap on keys":   {"b2_create_key", "transaction_cap_exceeded", "daily transaction cap", 403},
		"email":         {"b2_create_bucket", "email_not_verified", "email address verified", 401},
		"payment":       {"b2_create_bucket", "no_payment_history", "payment method", 401},
		"too many":      {"b2_create_bucket", "too_many_buckets", "most buckets", 400},
		"cap on bucket": {"b2_list_buckets", "transaction_cap_exceeded", "daily transaction cap", 403},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			s, _, f := newSetupServer(t)
			f.echo = testTempKey
			f.fail[c.call] = [2]any{c.status, c.code}
			rec := postJSON(t, s, "/api/backups/setup/storage/apply", applyBody("prod", "stage"))
			if !strings.Contains(rec.Body.String(), c.want) {
				t.Fatalf("%d %s", rec.Code, rec.Body.String())
			}
			if strings.Contains(rec.Body.String(), testTempKey) {
				t.Fatal("the message text echoed the key")
			}
		})
	}
}

func TestSetupApply_TemporaryKeyRules(t *testing.T) {
	t.Run("never expires: left alone, and it says so", func(t *testing.T) {
		s, _, f := newSetupServer(t)
		f.expiry = nil
		resp := applySetup(t, s, applyBody("prod"))
		if len(f.deleted) != 0 || !strings.Contains(resp.KeyNote, "never expires") {
			t.Fatalf("%v %q", f.deleted, resp.KeyNote)
		}
	})
	t.Run("no deleteKeys: left alone, and it says so", func(t *testing.T) {
		s, _, f := newSetupServer(t)
		f.caps = []string{"listBuckets", "writeBuckets", "writeKeys"}
		resp := applySetup(t, s, applyBody("prod"))
		if len(f.deleted) != 0 || !strings.Contains(resp.KeyNote, "cannot delete keys") {
			t.Fatalf("%v %q", f.deleted, resp.KeyNote)
		}
	})
	t.Run("delete fails: it says so", func(t *testing.T) {
		s, _, f := newSetupServer(t)
		f.fail["b2_delete_key"] = [2]any{500, "internal_error"}
		resp := applySetup(t, s, applyBody("prod"))
		if !resp.AllOK || !strings.Contains(resp.KeyNote, "could not delete your temporary key") {
			t.Fatalf("%q", resp.KeyNote)
		}
	})
}

func TestSetupApply_TemporaryKeyGoesNowhere(t *testing.T) {
	var logged bytes.Buffer
	log.SetOutput(&logged)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	s, c, f := newSetupServer(t)
	f.echo = testTempKey
	dir := t.TempDir()
	s.backups.path = filepath.Join(dir, "backups.json")
	s.backups.dataDir = dir

	var out bytes.Buffer
	check := postJSON(t, s, "/api/backups/setup/storage/check", map[string]any{"key_id": testTempKeyID, "application_key": testTempKey})
	out.Write(check.Body.Bytes())
	apply := postJSON(t, s, "/api/backups/setup/storage/apply", applyBody("prod", "stage"))
	out.Write(apply.Body.Bytes())
	// The failure paths too.
	f.fail["b2_authorize_account"] = [2]any{401, "bad_auth_token"}
	out.Write(postJSON(t, s, "/api/backups/setup/storage/apply", applyBody("prod")).Body.Bytes())
	delete(f.fail, "b2_authorize_account")
	f.fail["b2_create_key"] = [2]any{403, "transaction_cap_exceeded"}
	body := applyBody("prod")
	body["rotate"] = true
	out.Write(postJSON(t, s, "/api/backups/setup/storage/apply", body).Body.Bytes())

	if s.alerts.audit == nil {
		t.Fatal("this test needs the audit log")
	}
	audited, _ := json.Marshal(s.alerts.audit.Recent(200))
	if !strings.Contains(string(audited), "backups.setup.storage") {
		t.Fatal("the steps must be audited")
	}
	s.backups.mu.Lock()
	record, _ := json.Marshal(s.backups.rec)
	s.backups.mu.Unlock()
	sinks := map[string]string{"responses": out.String(), "logs": logged.String(), "audit": string(audited), "record": string(record)}
	for name, text := range sinks {
		for _, secret := range []string{testTempKey, testTempKeyID} {
			if strings.Contains(text, secret) {
				t.Errorf("the temporary key reached %s: %s", name, text)
			}
		}
		if strings.Contains(text, testMintSecret) {
			t.Errorf("a minted secret reached %s", name)
		}
	}
	// Nor disk, anywhere under the data directory.
	filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		raw, _ := os.ReadFile(path)
		for _, secret := range []string{testTempKey, testTempKeyID, testMintSecret} {
			if bytes.Contains(raw, []byte(secret)) {
				t.Errorf("%s holds %s", path, secret)
			}
		}
		return nil
	})
	// Nor argv, nor stdin: the temporary key is never given to the CLI.
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, call := range c.calls {
		text := strings.Join(call.args, " ") + call.stdin
		if strings.Contains(text, testTempKey) || strings.Contains(text, testTempKeyID) {
			t.Errorf("the temporary key reached the CLI: %v", call.args)
		}
		if strings.Contains(strings.Join(call.args, " "), testMintSecret) {
			t.Errorf("a minted secret reached argv: %v", call.args)
		}
	}
}

func TestSetupApply_OldOwnersAreReportedAndClearedOnlyWithConsent(t *testing.T) {
	s, c, _ := newSetupServer(t)
	cleared := []string{}
	c.hook = func(args []string, _ string) (*cli.Result, error, bool) {
		if !subsequence(args, []string{"backup-target", "owners"}) {
			return nil, nil, false
		}
		for i, a := range args {
			if a == "--clear" {
				cleared = append(cleared, args[i+1])
				if subsequence(args, []string{"--force"}) {
					return &cli.Result{Stdout: `{"fingerprint":"oldfp","removed":2,"skipped_locations":[{"prefix":"old","reason":"AccessDenied"}]}`}, nil, true
				}
				return &cli.Result{ExitCode: 1, Stderr: "a namespace written to in the last week needs --force", Stdout: `{"fingerprint":"oldfp","removed":0,"skipped_locations":[]}`}, nil, true
			}
		}
		return &cli.Result{Stdout: `{"markers":[{"namespace":"blog","fingerprint":"me","key":"k","self":true},{"namespace":"blog","fingerprint":"oldfp","key":"k2","self":false}],"skipped_locations":[]}`}, nil, true
	}
	resp := applySetup(t, s, applyBody("prod"))
	prod := resultFor(t, resp, "prod")
	if len(prod.OldOwners) != 1 || prod.OldOwners[0].Fingerprint != "oldfp" {
		t.Fatalf("%+v", prod)
	}
	if len(cleared) != 0 {
		t.Fatal("no clear without consent")
	}
	// A recent namespace needs the person to confirm the old server is gone.
	rec := postJSON(t, s, "/api/backups/setup/owners", map[string]any{"server": "prod", "fingerprint": "oldfp"})
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "last week") {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	rec = postJSON(t, s, "/api/backups/setup/owners", map[string]any{"server": "prod", "fingerprint": "oldfp", "force": true})
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "AccessDenied") {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	if rec := postJSON(t, s, "/api/backups/setup/owners", map[string]any{"server": "prod", "fingerprint": "-x y"}); rec.Code != http.StatusBadRequest {
		t.Fatalf("a bad fingerprint: %d", rec.Code)
	}
}

// ── recovery key ─────────────────────────────────────────────────────────────

func TestSetupRecovery_OnlyAPublicKey(t *testing.T) {
	s, _, _ := newSetupServer(t)
	for _, bad := range []string{"", "AGE-SECRET-KEY-1QQQ", "age1short"} {
		if rec := postJSON(t, s, "/api/backups/setup/recovery", map[string]any{"public_key": bad}); rec.Code != http.StatusBadRequest {
			t.Errorf("%q: %d", bad, rec.Code)
		}
	}
	if rec := postJSON(t, s, "/api/backups/setup/recovery", map[string]any{"public_key": testRecipient}); rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	// The same again is fine; another would strand backups made to the first.
	if rec := postJSON(t, s, "/api/backups/setup/recovery", map[string]any{"public_key": testRecipient}); rec.Code != http.StatusOK {
		t.Fatalf("%d", rec.Code)
	}
	other := "age1" + strings.Repeat("q", 58)
	if rec := postJSON(t, s, "/api/backups/setup/recovery", map[string]any{"public_key": other}); rec.Code != http.StatusConflict {
		t.Fatalf("%d", rec.Code)
	}
}

// ── turning on ───────────────────────────────────────────────────────────────

func setRecipient(s *Server) {
	s.backups.mu.Lock()
	defer s.backups.mu.Unlock()
	s.backups.rec.Recovery = &backupRecovery{PublicKey: testRecipient, ConfirmedAt: backupsNow.Add(-time.Hour)}
}

func policySetArgs(c *backupCLI, server string) []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, call := range c.calls {
		if subsequence(call.args, []string{"backup-policy", "set"}) && subsequence(call.args, []string{server}) {
			return call.args
		}
	}
	return nil
}

func TestSetupTurnOn_NeedsARecoveryKey(t *testing.T) {
	s, c, _ := newSetupServer(t)
	rec := postJSON(t, s, "/api/backups/setup/turn-on", map[string]any{"servers": []string{"prod"}, "start": "now"})
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "recovery key") {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	if c.countOf("backup-policy", "set") != 0 {
		t.Fatal("no plaintext policy from the simple flow")
	}
}

func TestSetupTurnOn_BackUpNowWritesThePolicyAndStartsADetachedRun(t *testing.T) {
	s, c, _ := newSetupServer(t)
	setRecipient(s)
	c.hook = func(args []string, _ string) (*cli.Result, error, bool) {
		if subsequence(args, []string{"backup", "run", "--all", "--detach"}) {
			return &cli.Result{Stdout: corpusFixtureFile(t, "detach-started.json")}, nil, true
		}
		return nil, nil, false
	}
	rec := postJSON(t, s, "/api/backups/setup/turn-on", map[string]any{"servers": []string{"prod"}, "start": "now", "adopt": true})
	if rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Servers []setupTurnOnResult `json:"servers"`
		AllOK   bool                `json:"all_ok"`
	}
	decodeData(t, rec, &out)
	r := out.Servers[0]
	if !out.AllOK || r.Run == nil || r.Run.Status != "started" || r.AllowEmpty {
		t.Fatalf("%+v", out)
	}
	args := strings.Join(policySetArgs(c, "prod"), " ")
	for _, want := range []string{"--interval 24h", "--recipient " + testRecipient, "--volumes=true", "--keep-daily=7", "--allow-empty=false", "--adopt redis-cache:redis:all"} {
		if !strings.Contains(args, want) {
			t.Errorf("policy args %q miss %q", args, want)
		}
	}
	if strings.Contains(args, "analytics-ch") {
		t.Error("an engine with no backup method cannot be adopted")
	}
	if c.countOf("stage") != 0 {
		t.Error("an unticked server is untouched")
	}
}

func TestSetupTurnOn_NoAdoptWithoutTheTick(t *testing.T) {
	s, c, _ := newSetupServer(t)
	setRecipient(s)
	postJSON(t, s, "/api/backups/setup/turn-on", map[string]any{"servers": []string{"prod"}, "start": "next"})
	if strings.Contains(strings.Join(policySetArgs(c, "prod"), " "), "--adopt") {
		t.Fatal("adopting needs the tick")
	}
}

func TestSetupTurnOn_NextRunStartsNothingAndAnEmptyServerAllowsEmpty(t *testing.T) {
	s, c, _ := newSetupServer(t)
	setRecipient(s)
	empty := `{"machine_interface":2,"host":"h","observed_at":"2026-11-04T03:17:04Z","target":{"configured":true,"bucket":"b","prefix":"p","has_credentials":true,"reachable":true},"policy":{"configured":false,"cron_installed":false,"retention":{},"volumes":false,"heartbeat_count":0},"encrypted":false,"aws_cli":true,"age_cli":true,"entries":[],"uncovered_databases":[]}`
	c.status["prod"] = func() (*cli.Result, error) { return &cli.Result{Stdout: empty}, nil }
	rec := postJSON(t, s, "/api/backups/setup/turn-on", map[string]any{"servers": []string{"prod"}, "start": "next"})
	if rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(strings.Join(policySetArgs(c, "prod"), " "), "--allow-empty=true") {
		t.Fatalf("a new empty server must not fail: %v", policySetArgs(c, "prod"))
	}
	if c.countOf("--detach") != 0 {
		t.Fatal("next scheduled run starts nothing")
	}
}

func TestSetupTurnOn_ServerWithoutStorageIsSkipped(t *testing.T) {
	s, c, _ := newSetupServer(t)
	setRecipient(s)
	c.status["prod"] = func() (*cli.Result, error) {
		return &cli.Result{Stdout: corpusFixtureFile(t, "status-unconfigured.json")}, nil
	}
	rec := postJSON(t, s, "/api/backups/setup/turn-on", map[string]any{"servers": []string{"prod"}, "start": "now"})
	var out struct {
		Servers []setupTurnOnResult `json:"servers"`
		AllOK   bool                `json:"all_ok"`
	}
	decodeData(t, rec, &out)
	if out.AllOK || !strings.Contains(out.Servers[0].Error, "no storage") || c.countOf("backup-policy", "set") != 0 {
		t.Fatalf("%+v", out)
	}
}

func TestSetupRun_ReportsEachDetachedOutcome(t *testing.T) {
	for fixture, want := range map[string]string{"detach-started.json": "started", "detach-already-running.json": "already_running", "detach-finished.json": "finished", "detach-failed.json": "failed"} {
		s, c, _ := newSetupServer(t)
		c.hook = func(args []string, _ string) (*cli.Result, error, bool) {
			if subsequence(args, []string{"--detach"}) {
				// finished and failed exit nonzero with a report.
				return &cli.Result{Stdout: corpusFixtureFile(t, fixture), ExitCode: 1}, nil, true
			}
			return nil, nil, false
		}
		rec := postJSON(t, s, "/api/backups/setup/run", map[string]any{"servers": []string{"prod"}})
		var out struct {
			Runs map[string]backupSetupRun `json:"runs"`
		}
		decodeData(t, rec, &out)
		got := out.Runs["prod"].Status
		if want == "finished" { // the fixture's run ended ok or not: both are terminal and truthful
			if got != "finished" && got != "failed" {
				t.Errorf("%s: %s", fixture, got)
			}
			continue
		}
		if got != want {
			t.Errorf("%s: %s", fixture, got)
		}
	}
}

func TestBackupStorageMultiplier(t *testing.T) {
	std := backupStatPolicy{Interval: "24h", Retention: backupRetention{KeepDaily: 7, KeepWeekly: 4, KeepMonthly: 12}}
	if got := backupStorageMultiplier(std, 14); got != 39 {
		t.Fatalf("14 days: %d", got)
	}
	if got := backupStorageMultiplier(std, 30); got != 55 {
		t.Fatalf("30 days: %d", got)
	}
}

// ── proving recovery ─────────────────────────────────────────────────────────

func seedEncryptedObject(s *Server, server string) string {
	key := "s3://b/teploy/" + server + "/blog/accessories/db/1.pgdump.age"
	env := backupStatusEnvelope{Host: server, ObservedAt: time.Now(), Entries: []backupEntry{{Name: "blog-db", Kind: "accessory", App: "blog", Accessory: "db", Coverage: "covered",
		Newest: &backupObject{Key: key, SizeBytes: 1 << 20, CreatedAt: time.Now().UTC().Format(time.RFC3339), Encrypted: true}, Encrypted: true}}, UncoveredDatabases: []string{}}
	s.recordBackupStatus(server, &env, nil, time.Now().UTC())
	return key
}

func TestSetupProof_ReadsThenRecordsObjectAndActor(t *testing.T) {
	s, c, _ := newSetupServer(t)
	if rec := postJSON(t, s, "/api/backups/setup/proof/start", map[string]any{}); rec.Code != http.StatusBadRequest {
		t.Fatalf("no key yet: %d", rec.Code)
	}
	s.backups.mu.Lock()
	s.backups.rec.Recovery = &backupRecovery{PublicKey: testRecipient, ConfirmedAt: time.Now().Add(-time.Hour)}
	s.backups.mu.Unlock()
	if rec := postJSON(t, s, "/api/backups/setup/proof/start", map[string]any{}); rec.Code != http.StatusConflict {
		t.Fatalf("no object yet: %d", rec.Code)
	}
	key := seedEncryptedObject(s, "prod")
	c.hook = func(args []string, _ string) (*cli.Result, error, bool) {
		if !subsequence(args, []string{"backup", "head"}) {
			return nil, nil, false
		}
		if !subsequence(args, []string{key, "--bytes", "131072", "--host", "prod", "--json"}) {
			t.Errorf("head args: %v", args)
		}
		raw, _ := json.Marshal(map[string]any{"machine_interface": 2, "host": "prod", "key": key, "encrypted": true, "requested_bytes": 131072, "length": 3, "total_size": 1 << 20, "data_base64": "AAEC"})
		return &cli.Result{Stdout: string(raw)}, nil, true
	}
	rec := postJSON(t, s, "/api/backups/setup/proof/start", map[string]any{})
	if rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	var start struct {
		Key  string `json:"key"`
		Data string `json:"data_base64"`
	}
	decodeData(t, rec, &start)
	if start.Key != key || start.Data != "AAEC" {
		t.Fatalf("%+v", start)
	}
	// Failure is recorded and proves nothing.
	if rec := postJSON(t, s, "/api/backups/setup/proof/result", map[string]any{"key": key, "passed": false, "detail": "no identity matched"}); rec.Code != http.StatusOK {
		t.Fatalf("%d", rec.Code)
	}
	s.backups.mu.Lock()
	passed := s.backups.rec.Recovery.DrillPassedAt
	s.backups.mu.Unlock()
	if passed != nil {
		t.Fatal("a failed proof is not a pass")
	}
	if rec := postJSON(t, s, "/api/backups/setup/proof/result", map[string]any{"key": "s3://b/other", "passed": true}); rec.Code != http.StatusBadRequest {
		t.Fatalf("another object: %d", rec.Code)
	}
	if rec := postJSON(t, s, "/api/backups/setup/proof/result", map[string]any{"key": key, "passed": true}); rec.Code != http.StatusOK {
		t.Fatalf("%d", rec.Code)
	}
	s.backups.mu.Lock()
	defer s.backups.mu.Unlock()
	proof := s.backups.rec.Setup.Proof
	if s.backups.rec.Recovery.DrillPassedAt == nil || proof == nil || proof.Object != key || proof.Server != "prod" || proof.Actor == "" {
		t.Fatalf("%+v", proof)
	}
}

func TestSetupProof_RefusesAnUnencryptedObject(t *testing.T) {
	s, c, _ := newSetupServer(t)
	setRecipient(s)
	s.backups.mu.Lock()
	s.backups.rec.Recovery.ConfirmedAt = time.Now().Add(-time.Hour)
	s.backups.mu.Unlock()
	key := seedEncryptedObject(s, "prod")
	c.hook = func(args []string, _ string) (*cli.Result, error, bool) {
		if subsequence(args, []string{"backup", "head"}) {
			raw, _ := json.Marshal(map[string]any{"machine_interface": 2, "host": "prod", "key": key, "encrypted": false, "length": 3, "data_base64": "AAEC"})
			return &cli.Result{Stdout: string(raw)}, nil, true
		}
		return nil, nil, false
	}
	if rec := postJSON(t, s, "/api/backups/setup/proof/start", map[string]any{}); rec.Code != http.StatusConflict {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
}

// ── retiring a server ────────────────────────────────────────────────────────

func retireBody(server, data string) map[string]any {
	return map[string]any{"key_id": testTempKeyID, "application_key": testTempKey, "server": server, "data": data}
}

func TestSetupRetire_DeletesTheKeyAndKeepsOrRemovesData(t *testing.T) {
	s, c, f := newSetupServer(t)
	f.expiry = nil
	applySetup(t, s, applyBody("prod", "stage"))
	s.backups.mu.Lock()
	prodKey := s.backups.rec.Setup.Keys["prod"].KeyID
	stageKey := s.backups.rec.Setup.Keys["stage"].KeyID
	s.backups.mu.Unlock()

	// An explicit choice, every time.
	if rec := postJSON(t, s, "/api/backups/setup/retire", map[string]any{"key_id": testTempKeyID, "application_key": testTempKey, "server": "prod"}); rec.Code != http.StatusBadRequest {
		t.Fatalf("%d", rec.Code)
	}
	rec := postJSON(t, s, "/api/backups/setup/retire", retireBody("prod", "keep"))
	if rec.Code != http.StatusOK || !strings.Contains(fmt.Sprint(f.deleted), prodKey) {
		t.Fatalf("%d %s %v", rec.Code, rec.Body.String(), f.deleted)
	}
	if f.count("b2_delete_file_version") != 0 {
		t.Fatal("keep means keep")
	}
	if c.countOf("backup-policy", "set", "--interval", "none") != 1 {
		t.Fatal("a server still in the fleet stops trying")
	}

	// Removal needs the name typed, and permissions.
	if rec := postJSON(t, s, "/api/backups/setup/retire", retireBody("stage", "remove")); rec.Code != http.StatusBadRequest {
		t.Fatalf("%d", rec.Code)
	}
	body := retireBody("stage", "remove")
	body["confirm"] = "stage"
	if rec := postJSON(t, s, "/api/backups/setup/retire", body); rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "deleteFiles") {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	f.caps = append(f.caps, "listFiles", "deleteFiles")
	rec = postJSON(t, s, "/api/backups/setup/retire", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(fmt.Sprint(f.deleted), stageKey) || f.count("b2_delete_file_version") != 1 {
		t.Fatalf("%v %d", f.deleted, f.count("b2_delete_file_version"))
	}
	if strings.Contains(fmt.Sprint(f.bodies["b2_list_file_versions"][0]["prefix"]), "prod") {
		t.Fatal("only this server's data")
	}
	raw, _ := json.Marshal(f.buckets)
	if strings.Contains(string(raw), "teploy/stage/.incoming/") {
		t.Fatalf("its rule goes too: %s", raw)
	}
	s.backups.mu.Lock()
	defer s.backups.mu.Unlock()
	if _, ok := s.backups.rec.Setup.Keys["stage"]; ok {
		t.Fatal("record stays")
	}
	if _, ok := s.backups.rec.Storage.Prefixes["stage"]; ok {
		t.Fatal("prefix stays")
	}
}

func TestSetupRetire_NeedsDeleteKeysAndARecord(t *testing.T) {
	s, _, f := newSetupServer(t)
	f.expiry = nil
	if rec := postJSON(t, s, "/api/backups/setup/retire", retireBody("prod", "keep")); rec.Code != http.StatusBadRequest {
		t.Fatalf("no record: %d", rec.Code)
	}
	applySetup(t, s, applyBody("prod"))
	f.caps = []string{"listBuckets"}
	if rec := postJSON(t, s, "/api/backups/setup/retire", retireBody("prod", "keep")); rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "deleteKeys") {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
}

// ── what "Protected" means ───────────────────────────────────────────────────

func statusWith(entries []backupEntry, encrypted bool) *backupStatusEnvelope {
	return &backupStatusEnvelope{Host: "h", ObservedAt: backupsNow,
		Target: backupStatTarget{Configured: true, Bucket: "b", HasCredentials: true, Reachable: true, IncomingLifecycle: "covered"},
		Policy: backupStatPolicy{Configured: true, Interval: "24h"}, Encrypted: encrypted, Entries: entries, UncoveredDatabases: []string{}}
}

func goodEntry(name, kind string) backupEntry {
	at := backupsNow.Add(-time.Hour)
	return backupEntry{Name: name, Kind: kind, App: "blog", Coverage: "covered", LastSuccess: &at, Encrypted: true,
		Newest: &backupObject{Key: "k", SizeBytes: 100, CreatedAt: at.Format(time.RFC3339), Encrypted: true}}
}

func setupStateOf(t *testing.T, s *Server, server string) backupSetupServerView {
	t.Helper()
	view := s.backupsView(context.Background(), backupsNow)
	for _, sv := range view.Setup.Servers {
		if sv.Server == server {
			return sv
		}
	}
	t.Fatalf("no state for %s", server)
	return backupSetupServerView{}
}

func TestSetupStatus_VolumeOnlyServerNeedsNoRestoreTest(t *testing.T) {
	s, _, _ := newSetupServer(t)
	s.recordBackupStatus("prod", statusWith([]backupEntry{goodEntry("blog/volumes", "volumes")}, true), nil, backupsNow)
	if got := setupStateOf(t, s, "prod"); got.State != setupProtected {
		t.Fatalf("%+v", got)
	}
	view := s.backupsView(context.Background(), backupsNow)
	if view.Setup.State != "protected" || view.Setup.ProjectedBytes != 100*view.Setup.Multiplier {
		t.Fatalf("%+v", view.Setup)
	}
}

func TestSetupStatus_ADatabaseNeedsAPassingRestoreTest(t *testing.T) {
	s, _, _ := newSetupServer(t)
	s.recordBackupStatus("prod", statusWith([]backupEntry{goodEntry("blog-db", "accessory")}, true), nil, backupsNow)
	if got := setupStateOf(t, s, "prod"); got.State != setupWaitingTest {
		t.Fatalf("no test yet: %+v", got)
	}
	test := &backupAutoTest{ID: "t1", Server: "prod", Container: "blog-db", App: "blog"}
	s.backups.mu.Lock()
	s.backups.rec.Tests["t1"] = test
	s.backups.mu.Unlock()
	if got := setupStateOf(t, s, "prod"); got.State != setupWaitingTest {
		t.Fatalf("never run: %+v", got)
	}
	// Repeated guard skips say why, instead of waiting silently.
	for i := 0; i < backupGuardSkipsShown; i++ {
		test.record(backupTestRun{At: backupsNow, Skipped: "waiting: 300 MB free RAM (< 1024)"})
	}
	if got := setupStateOf(t, s, "prod"); got.State != setupGuardSkipped || !strings.Contains(got.Detail, "300 MB") {
		t.Fatalf("%+v", got)
	}
	test.record(backupTestRun{At: backupsNow, OK: false, Detail: "boom"})
	if got := setupStateOf(t, s, "prod"); got.State != setupTestFailed {
		t.Fatalf("%+v", got)
	}
	test.record(backupTestRun{At: backupsNow, OK: true})
	if got := setupStateOf(t, s, "prod"); got.State != setupProtected {
		t.Fatalf("%+v", got)
	}
	// A skip after a pass does not undo the pass.
	test.record(backupTestRun{At: backupsNow, Skipped: "waiting"})
	if got := setupStateOf(t, s, "prod"); got.State != setupProtected {
		t.Fatalf("%+v", got)
	}
}

func TestSetupStatus_States(t *testing.T) {
	entry := func(mut func(*backupEntry)) []backupEntry {
		e := goodEntry("blog/volumes", "volumes")
		mut(&e)
		return []backupEntry{e}
	}
	cases := []struct {
		name string
		env  *backupStatusEnvelope
		err  error
		want string
	}{
		{"no target", func() *backupStatusEnvelope { e := statusWith(nil, false); e.Target = backupStatTarget{}; return e }(), nil, setupNotProtected},
		{"no policy", func() *backupStatusEnvelope { e := statusWith(nil, false); e.Policy = backupStatPolicy{}; return e }(), nil, setupNotProtected},
		{"empty", statusWith(nil, true), nil, setupEmpty},
		{"first backup", statusWith(entry(func(e *backupEntry) { e.Coverage, e.LastSuccess = "stale", nil }), true), nil, setupWaitingBackup},
		{"late", statusWith(entry(func(e *backupEntry) { e.Coverage = "stale" }), true), nil, setupLate},
		{"failing", statusWith(entry(func(e *backupEntry) { e.Coverage, e.LastError = "failing", "disk full" }), true), nil, setupFailing},
		{"not yet encrypted", statusWith(entry(func(e *backupEntry) { e.Encrypted = false }), true), nil, setupWaitingBackup},
		{"unread", nil, fmt.Errorf("ssh: timeout"), setupUnknown},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s, _, _ := newSetupServer(t)
			s.recordBackupStatus("prod", c.env, c.err, backupsNow)
			if got := setupStateOf(t, s, "prod"); got.State != c.want {
				t.Fatalf("%+v", got)
			}
			if s.backupsView(context.Background(), backupsNow).Setup.State == "protected" {
				t.Fatal("not protected")
			}
		})
	}
}

func TestSetupStatus_ACappedTargetIsNotUnreachable(t *testing.T) {
	s, _, _ := newSetupServer(t)
	env := statusWith(nil, true)
	env.Target.Reachable = false
	env.Target.Error = "An error occurred (TransactionCapExceeded) when calling ListObjectsV2"
	s.recordBackupStatus("prod", env, nil, backupsNow)
	got := setupStateOf(t, s, "prod")
	if got.State != setupCapped || !strings.Contains(got.Detail, "daily transaction cap") {
		t.Fatalf("%+v", got)
	}
	s.backups.mu.Lock()
	obs, _ := backupObservations(s.backups.rec.Servers["prod"], backupsNow)
	s.backups.mu.Unlock()
	found := false
	for _, o := range obs {
		if strings.Contains(o.rule.Key, ":target:") {
			found = true
			if strings.Contains(o.message, "cannot reach") || !strings.Contains(o.message, "daily transaction cap") {
				t.Fatalf("alert = %q", o.message)
			}
		}
	}
	if !found {
		t.Fatal("no target observation")
	}
}

func TestSetupStatus_NewAndRetiredServers(t *testing.T) {
	s, _, _ := newSetupServer(t)
	s.backups.mu.Lock()
	s.backups.rec.Storage = &backupStorage{Provider: "b2", Bucket: "b", Prefixes: map[string]string{}}
	s.backups.setupLocked().Keys["gone"] = &backupServerKey{KeyID: "K", KeyName: "teploy-gone-1", Bucket: "b", Prefix: "teploy/gone", State: backupKeyVerified}
	s.backups.rec.LastSweepAt = backupsNow
	s.backups.mu.Unlock()
	env := statusWith(nil, false)
	env.Target = backupStatTarget{}
	s.recordBackupStatus("compute-3", env, nil, backupsNow)
	view := s.backupsView(context.Background(), backupsNow)
	if fmt.Sprint(view.Setup.NewServers) != "[compute-3]" || fmt.Sprint(view.Setup.RetiredKeys) != "[gone]" {
		t.Fatalf("%+v", view.Setup)
	}
	if got := setupStateOf(t, s, "compute-3"); !strings.Contains(got.Detail, "compute-3 is not backed up") {
		t.Fatalf("%+v", got)
	}
}

func TestSetupStatus_LifecycleRecheckedOnEachRead(t *testing.T) {
	s, _, _ := newSetupServer(t)
	for lifecycle, opens := range map[string]bool{"absent": true, "covered": false} {
		env := statusWith(nil, true)
		env.Target.IncomingLifecycle = lifecycle
		s.recordBackupStatus("prod", env, nil, backupsNow)
		s.backups.mu.Lock()
		obs, _ := backupObservations(s.backups.rec.Servers["prod"], backupsNow)
		s.backups.mu.Unlock()
		failing := false
		for _, o := range obs {
			if strings.Contains(o.rule.Key, ":lifecycle:") {
				failing = !o.ok
			}
		}
		if failing != opens {
			t.Errorf("%s: failing = %v", lifecycle, failing)
		}
	}
	// A key that cannot read rules proves nothing either way.
	env := statusWith(nil, true)
	env.Target.IncomingLifecycle = "unknown"
	s.recordBackupStatus("prod", env, nil, backupsNow)
	s.backups.mu.Lock()
	obs, _ := backupObservations(s.backups.rec.Servers["prod"], backupsNow)
	s.backups.mu.Unlock()
	for _, o := range obs {
		if strings.Contains(o.rule.Key, ":lifecycle:") {
			t.Fatalf("unknown raises nothing: %+v", o)
		}
	}
}

// ── Dash's own volumes ───────────────────────────────────────────────────────

func TestPolicy_NoRecipientExcludesDashsOwnVolumes(t *testing.T) {
	s, c, _ := newSetupServer(t)
	s.backups.selfApp = "dash"
	c.policyShow["prod"] = `{"machine_interface":2,"configured":true,"excludes":["blog/volumes"]}`
	c.policyShow["stage"] = `{"machine_interface":2,"configured":true}`
	rec := postJSON(t, s, "/api/backups/policy", map[string]any{"interval": "24h", "preset": "simple", "volumes": true})
	if rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	args := strings.Join(policySetArgs(c, "prod"), " ")
	if !strings.Contains(args, "--exclude=blog/volumes,dash/volumes") {
		t.Fatalf("Dash's SSH key must not go up in plaintext: %s", args)
	}

	// Including them back is refused until there is a key.
	if rec := postJSON(t, s, "/api/backups/exclude", map[string]any{"server": "prod", "name": "dash/volumes", "excluded": false}); rec.Code != http.StatusBadRequest {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}

	// With a key, Dash's own entry comes back out, and a person's stays.
	setRecipient(s)
	c.mu.Lock()
	c.calls = nil
	c.mu.Unlock()
	c.policyShow["prod"] = `{"machine_interface":2,"configured":true,"excludes":["blog/volumes","dash/volumes"]}`
	rec = postJSON(t, s, "/api/backups/policy", map[string]any{"interval": "24h", "preset": "simple", "volumes": true})
	if rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	args = strings.Join(policySetArgs(c, "prod"), " ")
	if !strings.Contains(args, "--exclude=blog/volumes") || strings.Contains(args, "dash/volumes") {
		t.Fatalf("%s", args)
	}
}

func TestPolicy_AnEncryptedServerKeepsItsOwnVolumes(t *testing.T) {
	s, c, _ := newSetupServer(t)
	c.policyShow["prod"] = `{"machine_interface":2,"configured":true,"recipient":"` + testRecipient + `"}`
	c.policyShow["stage"] = `{"machine_interface":2,"configured":true,"recipient":"` + testRecipient + `"}`
	postJSON(t, s, "/api/backups/policy", map[string]any{"interval": "24h", "preset": "simple", "volumes": true})
	if strings.Contains(strings.Join(policySetArgs(c, "prod"), " "), "--exclude") {
		t.Fatal("a server encrypting already needs no exclude")
	}
}

func TestSetupState_PersistsWithoutSecrets(t *testing.T) {
	dir := t.TempDir()
	c := &backupCLI{status: statusesFromTestdata(t), tokens: []string{"backup-setup-v1"}}
	withTempGroupsFile(t)
	writeNotifications(t, `{}`)
	s := New(Config{DataDir: dir, NoAuth: true, CLIInstalled: func() bool { return true }, CLIRunner: c.run, CLIStdinRunner: c.runStdin, OperationMaxConcurrent: 4})
	_, client := newFakeBackblaze(t)
	s.backups.b2Client = client
	s.backups.probe = func(context.Context, s3probe.Config) s3probe.Result {
		return s3probe.Result{Write: true, Read: true, Delete: s3probe.DeleteRemoved, Hidden: true}
	}
	applySetup(t, s, applyBody("prod"))
	raw, err := os.ReadFile(filepath.Join(dir, "backups.json"))
	if err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(filepath.Join(dir, "backups.json"))
	if info.Mode().Perm() != 0600 {
		t.Fatalf("mode = %v", info.Mode())
	}
	for _, secret := range []string{testTempKey, testTempKeyID, testMintSecret} {
		if bytes.Contains(raw, []byte(secret)) {
			t.Fatalf("disk holds %s: %s", secret, raw)
		}
	}
	// It reloads.
	s2 := New(Config{DataDir: dir, NoAuth: true, CLIInstalled: func() bool { return true }, CLIRunner: c.run, CLIStdinRunner: c.runStdin, OperationMaxConcurrent: 4})
	s2.backups.mu.Lock()
	defer s2.backups.mu.Unlock()
	if s2.backups.rec.Setup == nil || s2.backups.rec.Setup.Keys["prod"] == nil {
		t.Fatalf("%+v", s2.backups.rec.Setup)
	}
}
