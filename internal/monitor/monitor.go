package monitor

import (
	"crypto/tls"
	"fmt"
	"log"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/useteploy/teploy-ui/internal/alert"
	"github.com/useteploy/teploy-ui/internal/store"
)

// Runner manages uptime monitors and runs checks on their intervals.
type Runner struct {
	store    store.Store
	alerter  *alert.Dispatcher
	client   *http.Client
	timers   map[string]*time.Ticker
	stopChs  map[string]chan struct{}
	lastStat map[string]string // last known status per monitor (for transition detection)
	mu       sync.Mutex
}

// New creates a monitor runner.
func New(st store.Store) *Runner {
	return &Runner{
		store:    st,
		lastStat: make(map[string]string),
		client: &http.Client{
			Timeout: 10 * time.Second,
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{InsecureSkipVerify: false},
			},
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				if len(via) >= 5 {
					return fmt.Errorf("too many redirects")
				}
				return nil
			},
		},
		timers:  make(map[string]*time.Ticker),
		stopChs: make(map[string]chan struct{}),
	}
}

// Start loads all monitors from the store and begins checking.
func (r *Runner) Start() {
	monitors, err := r.store.ListMonitors()
	if err != nil {
		log.Printf("[monitor] Failed to load monitors: %v", err)
		return
	}

	for _, m := range monitors {
		if m.Enabled {
			r.startMonitor(m)
		}
	}
	log.Printf("[monitor] Started %d monitors", len(monitors))
}

// SetAlerter configures the alert dispatcher for state-change notifications.
func (r *Runner) SetAlerter(d *alert.Dispatcher) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.alerter = d
}

// CheckNow runs a single check against the given monitor config without
// saving the result or firing alerts. Used by the "test monitor" button
// so users can verify configuration before enabling.
func (r *Runner) CheckNow(m store.Monitor) store.CheckResult {
	switch m.Type {
	case "http":
		return r.checkHTTP(m)
	case "tcp", "ping":
		return r.checkTCP(m)
	default:
		return store.CheckResult{
			MonitorID: m.ID,
			CheckedAt: time.Now(),
			Status:    "down",
			Message:   fmt.Sprintf("unknown monitor type: %s", m.Type),
		}
	}
}

// Stop stops all running monitors.
func (r *Runner) Stop() {
	r.mu.Lock()
	defer r.mu.Unlock()

	for id, ch := range r.stopChs {
		close(ch)
		if t, ok := r.timers[id]; ok {
			t.Stop()
		}
	}
	r.timers = make(map[string]*time.Ticker)
	r.stopChs = make(map[string]chan struct{})
}

// Reload reloads a single monitor (stop + start with new config).
func (r *Runner) Reload(m store.Monitor) {
	r.stopMonitor(m.ID)
	if m.Enabled {
		r.startMonitor(m)
	}
}

func (r *Runner) startMonitor(m store.Monitor) {
	r.mu.Lock()
	defer r.mu.Unlock()

	interval := m.Interval
	if interval < 10*time.Second {
		interval = 10 * time.Second
	}

	ticker := time.NewTicker(interval)
	stopCh := make(chan struct{})

	r.timers[m.ID] = ticker
	r.stopChs[m.ID] = stopCh

	go func() {
		// Run first check immediately
		r.runCheck(m)

		for {
			select {
			case <-ticker.C:
				r.runCheck(m)
			case <-stopCh:
				return
			}
		}
	}()
}

func (r *Runner) stopMonitor(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if ch, ok := r.stopChs[id]; ok {
		close(ch)
		delete(r.stopChs, id)
	}
	if t, ok := r.timers[id]; ok {
		t.Stop()
		delete(r.timers, id)
	}
}

func (r *Runner) runCheck(m store.Monitor) {
	var result store.CheckResult
	result.MonitorID = m.ID
	result.CheckedAt = time.Now()

	switch m.Type {
	case "http":
		result = r.checkHTTP(m)
	case "tcp":
		result = r.checkTCP(m)
	case "ping":
		result = r.checkTCP(m) // simplified: use TCP as ping substitute
	default:
		result.Status = "down"
		result.Message = fmt.Sprintf("unknown monitor type: %s", m.Type)
	}

	if err := r.store.SaveCheck(result); err != nil {
		log.Printf("[monitor] Failed to save check for %s: %v", m.ID, err)
	}

	// Fire alert on state transition (up->down or down->up).
	r.mu.Lock()
	prev := r.lastStat[m.ID]
	r.lastStat[m.ID] = result.Status
	alerter := r.alerter
	r.mu.Unlock()

	if alerter != nil && prev != "" && prev != result.Status {
		alerter.Send(alert.Event{
			MonitorID:   m.ID,
			MonitorName: m.Name,
			Status:      result.Status,
			Message:     result.Message,
			OccurredAt:  result.CheckedAt,
		})
	}
}

func (r *Runner) checkHTTP(m store.Monitor) store.CheckResult {
	result := store.CheckResult{
		MonitorID: m.ID,
		CheckedAt: time.Now(),
	}

	method := m.Method
	if method == "" {
		method = "GET"
	}

	req, err := http.NewRequest(method, m.Target, nil)
	if err != nil {
		result.Status = "down"
		result.Message = err.Error()
		return result
	}
	req.Header.Set("User-Agent", "teploy-ui/1.0")

	start := time.Now()
	resp, err := r.client.Do(req)
	result.ResponseTime = time.Since(start)

	if err != nil {
		result.Status = "down"
		result.Message = err.Error()
		return result
	}
	defer resp.Body.Close()

	result.StatusCode = resp.StatusCode

	expectedStatus := m.ExpectedStatus
	if expectedStatus == 0 {
		expectedStatus = 200
	}

	if resp.StatusCode >= 200 && resp.StatusCode < 400 {
		result.Status = "up"
	} else {
		result.Status = "down"
		result.Message = fmt.Sprintf("expected %d, got %d", expectedStatus, resp.StatusCode)
	}

	return result
}

func (r *Runner) checkTCP(m store.Monitor) store.CheckResult {
	result := store.CheckResult{
		MonitorID: m.ID,
		CheckedAt: time.Now(),
	}

	timeout := m.Timeout
	if timeout == 0 {
		timeout = 10 * time.Second
	}

	start := time.Now()
	conn, err := net.DialTimeout("tcp", m.Target, timeout)
	result.ResponseTime = time.Since(start)

	if err != nil {
		result.Status = "down"
		result.Message = err.Error()
		return result
	}
	conn.Close()

	result.Status = "up"
	return result
}
