package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/useteploy/teploy-dash/internal/caps"
	"github.com/useteploy/teploy-dash/internal/cli"
	"github.com/useteploy/teploy-dash/internal/operation"
	"github.com/useteploy/teploy-dash/internal/outbox"
	"github.com/useteploy/teploy-dash/internal/restoretest"
	"github.com/useteploy/teploy-dash/internal/store"
)

func TestCampaignScopedProjectAndDeletion(t *testing.T) {
	withTempGroupsFile(t)
	list := &atomic.Value{}
	list.Store(`{"prod":{"id":"srv-aaaaaaaaaaaaaaaa","host":"192.0.2.1"},"stage":{"id":"srv-bbbbbbbbbbbbbbbb","host":"192.0.2.2"}}`)
	s := groupBindingServer(t, list)
	saveGroupsFile(groupData{Groups: []groupEntry{{Name: "G", Apps: []string{"web"}, Projects: []projectEntry{{Name: "P"}}}}})
	for _, name := range []string{"prod", "stage"} {
		rec := groupPost(t, s, "/api/groups/G/projects/P/apps", `{"app":"web","server":"`+name+`"}`)
		if rec.Code != 200 {
			t.Fatalf("assign: %d %s", rec.Code, rec.Body.String())
		}
	}
	rec := groupDelete(t, s, "/api/groups/G/projects/P/apps/web?server_id=srv-aaaaaaaaaaaaaaaa")
	if rec.Code != 200 {
		t.Fatalf("delete: %d", rec.Code)
	}
	data, _ := loadGroupsFile()
	g := data.Groups[0]
	if len(g.ServerApps) != 2 || len(g.Projects[0].ServerApps) != 1 || g.Projects[0].ServerApps[0].ServerID != "srv-bbbbbbbbbbbbbbbb" {
		t.Fatalf("wrong binding changed: %+v", g)
	}
	list.Store(`{"renamed":{"id":"srv-bbbbbbbbbbbbbbbb","host":"192.0.2.2"},"stage":{"id":"srv-cccccccccccccccc","host":"192.0.2.3"}}`)
	rec = groupDelete(t, s, "/api/groups/G/apps/web?server_id=srv-bbbbbbbbbbbbbbbb")
	if rec.Code != 200 {
		t.Fatal(rec.Body.String())
	}
	data, _ = loadGroupsFile()
	if len(data.Groups[0].ServerApps) != 1 || len(data.Groups[0].Apps) != 1 {
		t.Fatal("scoped delete changed legacy membership")
	}
	rec = groupDelete(t, s, "/api/groups/G/apps/web?legacy=1")
	data, _ = loadGroupsFile()
	if rec.Code != 200 || len(data.Groups[0].Apps) != 0 || len(data.Groups[0].ServerApps) != 1 {
		t.Fatal("legacy delete changed scoped membership")
	}
}

func TestCampaignRestoreSavePreservesScheduleAndStrictDTO(t *testing.T) {
	st := store.NewFileStore(t.TempDir())
	runner := restoretest.New(st)
	defer runner.Stop(context.Background())
	rt := store.RestoreTest{ID: "rt", Server: "prod", App: "web", Accessory: "db", Bucket: "b", Region: "us-east-1", Enabled: true, IntervalHours: 24, LastRunAt: time.Now(), LastOK: true, LastDetail: "verified"}
	if err := st.SaveRestoreTest(rt); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	runner.SetTargetResolver(func(string) (restoretest.Target, error) {
		calls.Add(1)
		return restoretest.Target{}, errors.New("unexpected scheduled run")
	})
	s := New(Config{DataDir: t.TempDir(), NoAuth: true, Store: st, Restore: runner, CLIInstalled: func() bool { return false }})
	body := `{"id":"rt","server":"prod","app":"web","accessory":"db","bucket":"b","region":"us-east-1","enabled":true,"interval_hours":12}`
	rec := httptest.NewRecorder()
	s.handler().ServeHTTP(rec, httptest.NewRequest("POST", "/api/restore-tests", strings.NewReader(body)))
	var saved store.RestoreTest
	if err := json.Unmarshal(rec.Body.Bytes(), &saved); err != nil {
		t.Fatal(err)
	}
	if rec.Code != 200 || saved.LastRunAt.IsZero() || !saved.LastOK || saved.LastDetail != "verified" || saved.Incarnation == "" {
		t.Fatalf("saved projection: %d %+v", rec.Code, saved)
	}
	time.Sleep(25 * time.Millisecond)
	if calls.Load() != 0 {
		t.Fatal("save triggered immediate verification")
	}
	bad := httptest.NewRecorder()
	s.handler().ServeHTTP(bad, httptest.NewRequest("POST", "/api/restore-tests", strings.NewReader(strings.TrimSuffix(body, "}")+`,"last_ok":false}`)))
	if bad.Code != 400 {
		t.Fatalf("read-only field accepted: %d", bad.Code)
	}
}

func TestCampaignNotificationSaveRetainsDurableRestoreOutbox(t *testing.T) {
	withTempGroupsFile(t)
	st := store.NewFileStore(t.TempDir())
	runner := restoretest.New(st)
	defer runner.Stop(context.Background())
	dir := t.TempDir()
	opts := outbox.Options{ConfigFn: loadNotificationsConfig, Sender: &failingSender{err: "offline"}, PollInterval: time.Millisecond, BackoffBase: time.Hour, MaxAttempts: 5}
	ob, err := outbox.New(dir, opts)
	if err != nil {
		t.Fatal(err)
	}
	runner.SetAlerter(ob)
	rt := store.RestoreTest{ID: "rt", Server: "prod", App: "web", Accessory: "db", Bucket: "b", Region: "us-east-1", IntervalHours: 24}
	st.SaveRestoreTest(rt)
	stored, _ := st.GetRestoreTest("rt")
	// A resolution failure is a failed restore verdict, with no network or CLI.
	runner.SetTargetResolver(func(string) (restoretest.Target, error) {
		return restoretest.Target{}, errors.New("target unavailable")
	})
	s := &Server{restore: runner, store: st, outbox: ob}
	rec := httptest.NewRecorder()
	s.handleNotifications(rec, httptest.NewRequest("POST", "/api/notifications", strings.NewReader(`{"webhook_url":"https://alerts.example.test"}`)))
	if rec.Code != 200 {
		t.Fatal(rec.Body.String())
	}
	if _, err := runner.RunNow(*stored); err != nil {
		t.Fatal(err)
	}
	delivery := ob.LatestForRestoreTest("rt")
	if delivery == nil || delivery.Status != outbox.StatusPending {
		t.Fatal("restore did not enqueue durable alert")
	}
	ob.Start()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		d := ob.LatestForRestoreTest("rt")
		if d.Attempts > 0 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	ob.Stop(context.Background())
	reopened, err := outbox.New(dir, opts)
	if err != nil {
		t.Fatal(err)
	}
	after := reopened.LatestForRestoreTest("rt")
	if after == nil || after.ID != delivery.ID || after.Attempts != 1 {
		t.Fatalf("retry state not durable: %+v", after)
	}
}

func TestCampaignFutureHTTPHistoryAndSSE(t *testing.T) {
	dir := t.TempDir()
	id := "0123456789abcdef0123456789abcdef"
	records := filepath.Join(dir, "operations", "records")
	os.MkdirAll(records, 0700)
	record := `{"id":"` + id + `","record_version":999,"status":"failed","created_at":"2026-10-07T00:00:00Z","request":{"kind":"deploy","server":"prod","app":"web","image":"example/web:1"},"future":{"retain":true}}`
	path := filepath.Join(records, id+".json")
	os.WriteFile(path, []byte(record), 0600)
	s := New(Config{DataDir: dir, NoAuth: true, CLIInstalled: func() bool { return false }, OperationExecutor: func(context.Context, operation.Command, func(operation.Stream, string)) (int, error) {
		t.Error("executed")
		return 0, nil
	}})
	defer s.operations.Shutdown(context.Background())
	for _, route := range []string{"/api/operations", "/api/operations/" + id, "/api/operations/" + id + "/events"} {
		rec := httptest.NewRecorder()
		s.handler().ServeHTTP(rec, httptest.NewRequest("GET", route, nil))
		if rec.Code != 200 {
			t.Fatalf("GET %s: %d", route, rec.Code)
		}
		if strings.HasSuffix(route, "/events") && !strings.Contains(rec.Body.String(), "replay-complete") {
			t.Fatal("SSE did not finish replay")
		}
	}
	for _, route := range []string{"/api/operations", "/api/operations/" + id + "/retry", "/api/operations/" + id + "/cancel"} {
		rec := httptest.NewRecorder()
		s.handler().ServeHTTP(rec, httptest.NewRequest("POST", route, strings.NewReader(`{}`)))
		if rec.Code != 503 {
			t.Fatalf("POST %s: %d", route, rec.Code)
		}
	}
	after, _ := os.ReadFile(path)
	if string(after) != record {
		t.Fatal("future record rewritten")
	}
}

func TestCampaignCapabilityAlternateAdmissionAndOutput(t *testing.T) {
	s := newCapsServer(t, t.TempDir())
	seedPresetUsers(t, s)
	for _, route := range []string{"/api/deploy", "/api/manifests/prod/web/apply", "/api/operations"} {
		if !caps.NewSet(requiredCapabilities("POST", route)...).Allow(caps.ExecuteDeploy) {
			t.Fatalf("alternate admission unguarded: %s", route)
		}
	}
	rec := capDo(t, s, "v", RoleViewer, "GET", "/api/operations/nonexistent/events", "")
	if rec.Code != 403 || !strings.Contains(capError(t, rec), caps.ViewLogs) {
		t.Fatalf("viewer output read: %d", rec.Code)
	}
	rec = capDo(t, s, "v", RoleViewer, "GET", "/api/operations", "")
	if rec.Code != 200 {
		t.Fatal("metadata history denied")
	}
	request := withUser(httptest.NewRequest("POST", "/api/manifests/prod/web/apply", nil), &sessionInfo{caps: caps.NewSet(caps.ViewMetadata, caps.ExecuteMutate)})
	direct := httptest.NewRecorder()
	s.enqueueOperation(direct, request, operation.Request{Kind: operation.KindManifestApply, Server: "prod", App: "web"})
	if direct.Code != 403 {
		t.Fatalf("central admission: %d", direct.Code)
	}
}

func TestCampaignColdFleetSharesSweepAndRejectsInvalidatedResult(t *testing.T) {
	var sweeps atomic.Int32
	started := make(chan struct{})
	release := make(chan struct{})
	s := New(Config{DataDir: t.TempDir(), NoAuth: true, CLIInstalled: func() bool { return true }, CLIRunner: func(_ context.Context, args ...string) (*cli.Result, error) {
		if args[0] == "server" {
			return &cli.Result{Stdout: `{"prod":{"host":"192.0.2.1"}}`}, nil
		}
		if args[0] == "app" && args[1] == "list" {
			if sweeps.Add(1) == 1 {
				close(started)
				<-release
			}
			return &cli.Result{Stdout: fleetAppListJSON("192.0.2.1", "web")}, nil
		}
		return &cli.Result{}, nil
	}})

	var wg sync.WaitGroup
	errorsCh := make(chan error, 12)
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _, err := s.coldFleet(context.Background()); errorsCh <- err }()
	}
	<-started
	s.fleet.invalidate()
	close(release)
	wg.Wait()
	close(errorsCh)
	for err := range errorsCh {
		if err != nil {
			t.Fatal(err)
		}
	}
	if sweeps.Load() != 2 {
		t.Fatalf("expected invalidated sweep plus one shared retry, got %d", sweeps.Load())
	}
}

func TestCampaignServersNormalizeBothErasAndRejectUnknown(t *testing.T) {
	for _, raw := range []string{
		`{"prod":{"id":"srv-aaaaaaaaaaaaaaaa","host":"127.0.0.1:1","user":"deploy","role":"app"}}`,
		`{"machine_interface":2,"observed_at":"2026-10-07T00:00:00Z","servers":[{"name":"prod","id":"srv-aaaaaaaaaaaaaaaa","host":"127.0.0.1:1","user":"deploy","role":"app"}]}`,
		`{"machine_interface":999,"servers":[]}`,
		`{"machine_interface":2,"servers":{}}`,
	} {
		s := New(Config{DataDir: t.TempDir(), NoAuth: true, CLIInstalled: func() bool { return true }, CLIRunner: func(context.Context, ...string) (*cli.Result, error) { return &cli.Result{Stdout: raw}, nil }})
		rec := httptest.NewRecorder()
		s.handleServers(rec, httptest.NewRequest("GET", "/api/servers", nil))
		if strings.Contains(raw, `"machine_interface":999`) || strings.Contains(raw, `"servers":{}`) {
			if rec.Code != 502 {
				t.Fatalf("malformed accepted: %d", rec.Code)
			}
			continue
		}
		var envelope struct {
			Data map[string]map[string]interface{} `json:"data"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
			t.Fatal(err)
		}
		if rec.Code != 200 || len(envelope.Data) != 1 || envelope.Data["prod"]["id"] != "srv-aaaaaaaaaaaaaaaa" || envelope.Data["prod"]["host"] != "127.0.0.1:1" {
			t.Fatalf("wrong normalized servers: %+v", envelope)
		}
	}
}

func TestCampaignMCPSourceOwnedEnvRefuses(t *testing.T) {
	s, cookie := newAuthorityServer(t)
	putManifest(t, s, cookie, "prod", "web", `{"mode":"git-managed","git":{"repository":"https://github.com/acme/web","revision":"0123456789abcdef0123456789abcdef01234567"},"manifest":"app: web\nimage: example/web:1\nenv:\n  RAILS_ENV: production\n"}`)
	backend := mcpBackend{s: s}
	for _, call := range []func() (string, error){func() (string, error) {
		return backend.SetEnv(context.Background(), "prod", "web", "RAILS_ENV", "test")
	}, func() (string, error) { return backend.UnsetEnv(context.Background(), "prod", "web", "RAILS_ENV") }} {
		_, err := call()
		if err == nil || !strings.Contains(err.Error(), "git-managed") {
			t.Fatalf("source-owned MCP edit: %v", err)
		}
	}
}
