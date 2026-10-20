package source

// routing.go — the D4 repo→app routing cache. Webhook relays need to know
// which app(s) a push should wake before any SSH work happens; the app's
// config on its server is the authority, so this file holds ROUTING HINTS
// only: every relay re-verifies the candidate against `config get --json`
// before polling, and a mismatch removes the hint. Entries are recorded
// whenever dash observes an app's git source (connect flows, source show)
// and survive restarts so a rebooted dashboard still relays.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/useteploy/teploy-dash/internal/durable"
)

// maxRoutes bounds the hint file; routes are per (server, app), so this is
// an order of magnitude beyond a real fleet.
const maxRoutes = 4096

// Route is one cached repo→app link. Server is the dash-known server name
// (routing hint only — the relay resolves it through the CLI like every
// app-scoped call). Repository is the canonical clone URL.
type Route struct {
	Server     string    `json:"server"`
	App        string    `json:"app"`
	Repository string    `json:"repository"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// Routing persists routes under <dataDir>/sources/routing.json beside the
// per-source stores.
type Routing struct {
	mu     sync.Mutex
	path   string
	routes []Route
}

func OpenRouting(dataDir string) (*Routing, error) {
	if dataDir == "" {
		return nil, fmt.Errorf("source data directory is required")
	}
	root, err := filepath.Abs(filepath.Join(dataDir, "sources"))
	if err != nil {
		return nil, err
	}
	routing := &Routing{path: filepath.Join(root, "routing.json")}
	data, err := os.ReadFile(routing.path)
	if errors.Is(err, os.ErrNotExist) {
		return routing, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read source routing: %w", err)
	}
	if len(data) == 0 {
		return routing, nil
	}
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&routing.routes); err != nil {
		return nil, fmt.Errorf("parse source routing: %w", err)
	}
	var extra interface{}
	if err := decoder.Decode(&extra); err != io.EOF {
		return nil, fmt.Errorf("parse source routing: trailing content")
	}
	return routing, nil
}

// Link records (or refreshes) the route for one app. The repository is
// canonicalized so a push matches under any spelling.
func (r *Routing) Link(server, app, repository string) error {
	canonical, err := CanonicalURL(repository)
	if err != nil {
		return err
	}
	if server == "" || app == "" {
		return fmt.Errorf("route needs a server and an app")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now().UTC()
	for i := range r.routes {
		if r.routes[i].Server == server && r.routes[i].App == app {
			r.routes[i].Repository = canonical
			r.routes[i].UpdatedAt = now
			return r.persist()
		}
	}
	r.routes = append(r.routes, Route{Server: server, App: app, Repository: canonical, UpdatedAt: now})
	if len(r.routes) > maxRoutes {
		r.routes = r.routes[len(r.routes)-maxRoutes:]
	}
	return r.persist()
}

// Unlink forgets one app's route (its config no longer declares the
// repository, or the app is gone).
func (r *Routing) Unlink(server, app string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	kept := r.routes[:0]
	for _, route := range r.routes {
		if route.Server != server || route.App != app {
			kept = append(kept, route)
		}
	}
	if len(kept) == len(r.routes) {
		return nil
	}
	r.routes = kept
	return r.persist()
}

// RoutesFor returns the apps cached against one repository (canonical URL
// match), oldest link first so relays wake the longest-connected app
// first when several share a repository.
func (r *Routing) RoutesFor(repository string) []Route {
	canonical, err := CanonicalURL(repository)
	if err != nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []Route
	for _, route := range r.routes {
		if route.Repository == canonical {
			out = append(out, route)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].UpdatedAt.Before(out[j].UpdatedAt) })
	return out
}

func (r *Routing) persist() error {
	data, err := json.MarshalIndent(r.routes, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(r.path), 0700); err != nil {
		return fmt.Errorf("create source routing directory: %w", err)
	}
	return durable.Replace(r.path, append(data, '\n'), 0600)
}
