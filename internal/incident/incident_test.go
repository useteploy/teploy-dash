package incident

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/useteploy/teploy-dash/internal/alert"
)

type sink struct {
	mu         sync.Mutex
	sent       []alert.Event
	suppressed []string
}

func (s *sink) Send(ev alert.Event) {
	s.mu.Lock()
	s.sent = append(s.sent, ev)
	s.mu.Unlock()
}
func (s *sink) RecordSuppressed(ev alert.Event, note string) {
	s.mu.Lock()
	s.suppressed = append(s.suppressed, note)
	s.mu.Unlock()
}
func (s *sink) kinds() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var k []string
	for _, ev := range s.sent {
		k = append(k, ev.Kind)
	}
	return strings.Join(k, ",")
}

var t0 = time.Date(2026, 11, 3, 22, 0, 0, 0, time.UTC)

func monitorRule() Rule {
	return Rule{Source: alert.SourceMonitor, ID: "m1", Name: "api", App: "shop", Server: "deploy-1",
		Target: "https://shop.example/", Path: "/monitors/m1", Policy: Policy{FailAfter: 3, RecoverAfter: 2}}
}

func newTracker(t *testing.T, s *sink, mod func(*Options)) *Tracker {
	t.Helper()
	opts := Options{Alerter: s, PublicURL: func() string { return "https://dash.example/" }}
	if mod != nil {
		mod(&opts)
	}
	tr, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	return tr
}

func observe(tr *Tracker, r Rule, pattern string, start time.Time) time.Time {
	at := start
	for _, c := range pattern {
		at = at.Add(time.Minute)
		tr.Observe(r, c == '+', at, map[bool]string{true: "ok", false: "HTTP 502"}[c == '+'])
	}
	return at
}

// Debounce: two failures are nothing, the third opens exactly once, more
// failures do not re-open; one pass does not recover, two do.
func TestDebounceOpensOnceAndRecoversAfterPasses(t *testing.T) {
	s := &sink{}
	tr := newTracker(t, s, nil)
	r := monitorRule()
	at := observe(tr, r, "--", t0)
	if s.kinds() != "" {
		t.Fatalf("2 failures sent %s", s.kinds())
	}
	at = observe(tr, r, "----", at)
	if s.kinds() != alert.KindOpened {
		t.Fatalf("after 6 failures: %s, want one open", s.kinds())
	}
	open := s.sent[0]
	if open.IncidentID == "" || open.ConsecutiveFailures != 3 || !open.StartedAt.Equal(t0.Add(time.Minute)) ||
		open.DurationS != 0 || open.URL != "https://dash.example/monitors/m1" || open.Status != "down" || open.Version != 2 {
		t.Fatalf("open event = %+v", open)
	}
	at = observe(tr, r, "+-+", at)
	if s.kinds() != alert.KindOpened {
		t.Fatalf("flapping +-+ must not recover: %s", s.kinds())
	}
	observe(tr, r, "+", at)
	if s.kinds() != alert.KindOpened+","+alert.KindRecovered {
		t.Fatalf("two passes must recover: %s", s.kinds())
	}
	rec := s.sent[1]
	if rec.IncidentID != open.IncidentID || rec.Status != "up" || rec.Reason != "" || rec.DurationS <= 0 {
		t.Fatalf("recovery = %+v", rec)
	}
	if len(tr.Open()) != 0 {
		t.Fatal("incident still open after recovery")
	}
}

// A new outage gets a new incident id.
func TestEachOutageIsANewIncident(t *testing.T) {
	s := &sink{}
	tr := newTracker(t, s, nil)
	r := monitorRule()
	at := observe(tr, r, "---++", t0)
	observe(tr, r, "---", at)
	if s.kinds() != "incident_opened,incident_recovered,incident_opened" || s.sent[0].IncidentID == s.sent[2].IncidentID {
		t.Fatalf("events %s ids %s/%s", s.kinds(), s.sent[0].IncidentID, s.sent[2].IncidentID)
	}
}

// Recovery is sent only when the open was sent: an incident that opened
// with no channel configured recovers silently.
func TestRecoveryOnlyIfOpenWasSent(t *testing.T) {
	s := &sink{}
	channels := false
	tr := newTracker(t, s, func(o *Options) { o.HasChannels = func() bool { return channels } })
	r := monitorRule()
	at := observe(tr, r, "---", t0)
	if len(tr.Open()) != 1 || s.kinds() != "" {
		t.Fatalf("unannounced incident: open=%d sent=%s", len(tr.Open()), s.kinds())
	}
	channels = true
	observe(tr, r, "++", at)
	if s.kinds() != "" {
		t.Fatalf("recovery of an unsent open was sent: %s", s.kinds())
	}
}

func TestRepeatEvery(t *testing.T) {
	s := &sink{}
	tr := newTracker(t, s, nil)
	r := monitorRule()
	r.Policy.RepeatEvery = 3 * time.Minute
	observe(tr, r, "---------", t0) // open at minute 3, repeats at 6 and 9
	if s.kinds() != "incident_opened,incident_repeat,incident_repeat" {
		t.Fatalf("repeat: %s", s.kinds())
	}
	if s.sent[1].IncidentID != s.sent[0].IncidentID || s.sent[1].DurationS == 0 || s.sent[1].Status != "down" {
		t.Fatalf("repeat event = %+v", s.sent[1])
	}
}

// Retiring an announced incident sends recovered reason "retired".
func TestRetireSendsRetired(t *testing.T) {
	s := &sink{}
	tr := newTracker(t, s, nil)
	r := monitorRule()
	observe(tr, r, "---", t0)
	tr.Retire("monitor:m1", "")
	if s.kinds() != "incident_opened,incident_recovered" || s.sent[1].Reason != alert.ReasonRetired || s.sent[1].Status != "up" {
		t.Fatalf("retire: %s %+v", s.kinds(), s.sent)
	}
	tr.Retire("monitor:m1", "") // idempotent
	if len(s.sent) != 2 {
		t.Fatal("second retire sent again")
	}
}

// Restart: open incidents persist; counts are restored from history (Seed),
// so the restarted tracker neither re-opens nor forgets, and a monitor that
// was failing before the restart opens on the next failure, dated from the
// first failing check.
func TestRestartRestoresIncidentAndCounts(t *testing.T) {
	dir := t.TempDir()
	s := &sink{}
	tr := newTracker(t, s, func(o *Options) { o.Dir = dir })
	tr.EndAdoption() // a fresh dir is a first run; nothing to adopt
	r := monitorRule()
	observe(tr, r, "----", t0)

	s2 := &sink{}
	tr2 := newTracker(t, s2, func(o *Options) { o.Dir = dir })
	if open := tr2.Open(); len(open) != 1 || open[0].ID != s.sent[0].IncidentID {
		t.Fatalf("persisted incidents = %+v", open)
	}
	tr2.Seed(r, []Sample{{false, t0.Add(4 * time.Minute)}, {false, t0.Add(3 * time.Minute)}, {false, t0.Add(2 * time.Minute)}})
	observe(tr2, r, "-", t0.Add(4*time.Minute))
	if s2.kinds() != "" {
		t.Fatalf("restart re-sent: %s", s2.kinds())
	}
	observe(tr2, r, "++", t0.Add(10*time.Minute))
	if s2.kinds() != alert.KindRecovered || s2.sent[0].IncidentID != s.sent[0].IncidentID {
		t.Fatalf("recovery after restart: %s", s2.kinds())
	}

	// Never-opened rule with 2 failures in history: the next failure is the
	// third and opens, StartedAt from history.
	s3 := &sink{}
	tr3 := newTracker(t, s3, nil)
	tr3.Seed(r, []Sample{{false, t0.Add(2 * time.Minute)}, {false, t0.Add(time.Minute)}, {true, t0}})
	tr3.Observe(r, false, t0.Add(3*time.Minute), "x")
	if s3.kinds() != alert.KindOpened || !s3.sent[0].StartedAt.Equal(t0.Add(time.Minute)) || s3.sent[0].ConsecutiveFailures != 3 {
		t.Fatalf("seeded open: %s %+v", s3.kinds(), s3.sent)
	}
	// A passing streak seeds oks: one more pass recovers a persisted incident.
	tr3.Seed(r, []Sample{{true, t0.Add(4 * time.Minute)}})
	tr3.Observe(r, true, t0.Add(5*time.Minute), "ok")
	if s3.kinds() != "incident_opened,incident_recovered" {
		t.Fatalf("seeded oks: %s", s3.kinds())
	}
}

// Hold: failures during a hold keep counting, the hold is recorded once,
// and if still failing after it ends the incident opens immediately (dated
// from the first failure). A hold that ends with the rule healthy sends
// nothing.
func TestHoldIsHeldNotDropped(t *testing.T) {
	s := &sink{}
	held := true
	tr := newTracker(t, s, func(o *Options) {
		o.Hold = func(Rule, time.Time) (bool, string) { return held, "deploy of shop running" }
	})
	r := monitorRule()
	at := observe(tr, r, "-----", t0)
	if s.kinds() != "" || len(s.suppressed) != 1 || s.suppressed[0] != "held: deploy of shop running" {
		t.Fatalf("during hold: sent=%s suppressed=%v", s.kinds(), s.suppressed)
	}
	if h := tr.HeldRules(); len(h) != 1 || h[0].ConsecutiveFailures != 5 {
		t.Fatalf("held rules = %+v", h)
	}
	held = false
	observe(tr, r, "-", at)
	if s.kinds() != alert.KindOpened || s.sent[0].ConsecutiveFailures != 6 || !s.sent[0].StartedAt.Equal(t0.Add(time.Minute)) {
		t.Fatalf("after hold: %s %+v", s.kinds(), s.sent)
	}

	// Healthy when the hold ends: nothing.
	s2 := &sink{}
	held = true
	tr2 := newTracker(t, s2, func(o *Options) {
		o.Hold = func(Rule, time.Time) (bool, string) { return held, "maintenance" }
	})
	at = observe(tr2, r, "---", t0)
	held = false
	observe(tr2, r, "+--", at)
	if s2.kinds() != "" {
		t.Fatalf("2 failures after a healthy end must not open: %s", s2.kinds())
	}
}

// NoHold rules ignore holds but not silences.
func TestNoHoldStillSilenced(t *testing.T) {
	s := &sink{}
	silenced := false
	tr := newTracker(t, s, func(o *Options) {
		o.Hold = func(Rule, time.Time) (bool, string) { return true, "maintenance" }
		o.Silenced = func(Rule, time.Time) (bool, string) { return silenced, "until 23:00" }
	})
	r := Rule{Source: alert.SourceOperation, ID: "op-deploy-s-a", Name: "deploy a", NoHold: true, Policy: Policy{FailAfter: 1, RecoverAfter: 1}}
	tr.Observe(r, false, t0, "exit 1")
	if s.kinds() != alert.KindOpened {
		t.Fatalf("NoHold rule held: %s", s.kinds())
	}
	silenced = true
	r.ID = "op-deploy-s-b"
	tr.Observe(r, false, t0, "exit 1")
	if s.kinds() != alert.KindOpened || len(s.suppressed) != 1 || !strings.HasPrefix(s.suppressed[0], "silenced:") {
		t.Fatalf("silence ignored: %s %v", s.kinds(), s.suppressed)
	}
}

// A silence closes what it covers with suppressed_end.
func TestCloseMatchingSuppressedEnd(t *testing.T) {
	s := &sink{}
	tr := newTracker(t, s, nil)
	observe(tr, monitorRule(), "---", t0)
	n := tr.CloseMatching(func(i Incident) bool { return i.App == "shop" }, alert.ReasonSuppressedEnd, "silenced")
	if n != 1 || s.kinds() != "incident_opened,incident_recovered" || s.sent[1].Reason != alert.ReasonSuppressedEnd {
		t.Fatalf("close matching: n=%d %s", n, s.kinds())
	}
}

// One-shot rules (operations) open on the failure, recover on the next
// success, and otherwise auto-resolve as retired.
func TestOneShotAutoResolve(t *testing.T) {
	s := &sink{}
	now := t0
	tr := newTracker(t, s, func(o *Options) { o.Now = func() time.Time { return now } })
	r := Rule{Source: alert.SourceOperation, ID: "op-deploy-s-a", Name: "deploy a", NoHold: true,
		Policy: Policy{FailAfter: 1, RecoverAfter: 1, AutoResolveAfter: time.Hour}}
	tr.Observe(r, false, t0, "exit 1")
	now = t0.Add(59 * time.Minute)
	tr.Sweep()
	if s.kinds() != alert.KindOpened {
		t.Fatalf("swept early: %s", s.kinds())
	}
	now = t0.Add(61 * time.Minute)
	tr.Sweep()
	if s.kinds() != "incident_opened,incident_recovered" || s.sent[1].Reason != alert.ReasonRetired {
		t.Fatalf("auto-resolve: %s", s.kinds())
	}
	tr.Observe(r, false, now, "exit 1")
	tr.Observe(r, true, now.Add(time.Minute), "deploy succeeded")
	if s.kinds() != "incident_opened,incident_recovered,incident_opened,incident_recovered" || s.sent[3].Reason != "" {
		t.Fatalf("success recovers: %s", s.kinds())
	}
}

// First-enable summary: several incidents opened with no channel become ONE
// human summary plus machine-only opens; afterwards their recoveries flow.
func TestAnnounceUnannouncedSummary(t *testing.T) {
	s := &sink{}
	channels := false
	tr := newTracker(t, s, func(o *Options) { o.HasChannels = func() bool { return channels } })
	for _, id := range []string{"a", "b", "c", "d"} {
		r := monitorRule()
		r.ID, r.Name = id, id+".example"
		observe(tr, r, "---", t0)
	}
	if tr.AnnounceUnannounced() != 0 {
		t.Fatal("announced with no channel")
	}
	channels = true
	if n := tr.AnnounceUnannounced(); n != 4 {
		t.Fatalf("announced %d", n)
	}
	var human, machine int
	for _, ev := range s.sent {
		switch ev.Audience {
		case alert.AudienceHuman:
			human++
			if ev.Kind != alert.KindSummary || !strings.Contains(ev.Message, "d.example") {
				t.Fatalf("summary = %+v", ev)
			}
		case alert.AudienceMachine:
			machine++
			if ev.Kind != alert.KindOpened || ev.IncidentID == "" {
				t.Fatalf("machine open = %+v", ev)
			}
		default:
			t.Fatalf("unrouted event %+v", ev)
		}
	}
	if human != 1 || machine != 4 {
		t.Fatalf("human=%d machine=%d", human, machine)
	}
	if tr.AnnounceUnannounced() != 0 {
		t.Fatal("announced twice")
	}
	r := monitorRule()
	r.ID = "a"
	observe(tr, r, "++", t0.Add(time.Hour))
	if last := s.sent[len(s.sent)-1]; last.Kind != alert.KindRecovered {
		t.Fatalf("recovery after summary: %+v", last)
	}
}

// First run after the upgrade (no incidents.json): outages that already
// existed are adopted unannounced and announced as ONE summary; a corrupt
// file is set aside and behaves like a first run.
func TestFirstRunAdoptionSummarizes(t *testing.T) {
	dir := t.TempDir()
	s := &sink{}
	tr := newTracker(t, s, func(o *Options) { o.Dir = dir })
	if !tr.Adopting() {
		t.Fatal("missing incidents.json must start adopting")
	}
	for _, id := range []string{"a", "b", "c"} {
		r := monitorRule()
		r.ID, r.Key = id, "monitor:"+id
		tr.AdoptOpen(r, t0, 5, "HTTP 502")
	}
	r := monitorRule()
	r.ID, r.Key = "d", "monitor:d"
	observe(tr, r, "---", t0) // opens during adoption: unannounced too
	if len(s.sent) != 0 {
		t.Fatalf("adoption sent %s", s.kinds())
	}
	tr.AdoptOpen(r, t0, 9, "x") // already open: no-op
	if n := tr.EndAdoption(); n != 4 {
		t.Fatalf("announced %d", n)
	}
	human := 0
	for _, ev := range s.sent {
		if ev.Audience == alert.AudienceHuman {
			human++
		}
	}
	if human != 1 || len(s.sent) != 5 {
		t.Fatalf("want one summary + 4 machine opens, got %s", s.kinds())
	}
	if tr.Adopting() {
		t.Fatal("still adopting")
	}
	tr2 := newTracker(t, &sink{}, func(o *Options) { o.Dir = dir })
	if tr2.Adopting() || len(tr2.Open()) != 4 {
		t.Fatalf("second start: adopting=%v open=%d", tr2.Adopting(), len(tr2.Open()))
	}

	if err := os.WriteFile(filepath.Join(dir, "incidents.json"), []byte("{not json"), 0600); err != nil {
		t.Fatal(err)
	}
	tr3, err := New(Options{Dir: dir})
	if err != nil || !tr3.Adopting() || len(tr3.Open()) != 0 {
		t.Fatalf("corrupt file: err=%v adopting=%v", err, tr3 != nil && tr3.Adopting())
	}
	aside, _ := filepath.Glob(filepath.Join(dir, "incidents.json.corrupt-*"))
	if len(aside) != 1 {
		t.Fatalf("corrupt file not set aside: %v", aside)
	}
}

// A held rule opens on the release pass once its hold ends, with the count
// and message it had, without another observation.
func TestReleaseHoldsOpensWithoutNextObservation(t *testing.T) {
	s := &sink{}
	held := true
	tr := newTracker(t, s, func(o *Options) {
		o.Hold = func(Rule, time.Time) (bool, string) { return held, "deploy running" }
	})
	r := Rule{Source: alert.SourceRestoreTest, ID: "rt1", Name: "restore", App: "shop", Policy: Policy{FailAfter: 1, RecoverAfter: 1}}
	tr.Observe(r, false, t0, "verify failed")
	if tr.ReleaseHolds() != 0 || len(s.sent) != 0 {
		t.Fatal("released while still held")
	}
	held = false
	if tr.ReleaseHolds() != 1 || s.kinds() != alert.KindOpened || s.sent[0].Message != "verify failed" || s.sent[0].ConsecutiveFailures != 1 {
		t.Fatalf("release: %s %+v", s.kinds(), s.sent)
	}
	if tr.ReleaseHolds() != 0 {
		t.Fatal("released twice")
	}
}
