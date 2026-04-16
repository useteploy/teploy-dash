package fleet

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// Node represents a remote teploy-ui instance.
type Node struct {
	Name    string `json:"name"`
	URL     string `json:"url"` // e.g. "http://10.0.0.2:3456"
	Healthy bool   `json:"healthy"`
	LastSeen time.Time `json:"last_seen"`
}

// NodeStatus holds the status fetched from a remote node.
type NodeStatus struct {
	Node     Node          `json:"node"`
	Apps     []interface{} `json:"apps"`
	Monitors []interface{} `json:"monitors"`
	Error    string        `json:"error,omitempty"`
}

// Aggregator pulls status from multiple teploy-ui instances.
type Aggregator struct {
	nodes  []Node
	client *http.Client
	mu     sync.RWMutex
}

// New creates a fleet aggregator.
func New(nodes []Node) *Aggregator {
	return &Aggregator{
		nodes: nodes,
		client: &http.Client{Timeout: 5 * time.Second},
	}
}

// FetchAll queries all nodes in parallel and returns their status.
func (a *Aggregator) FetchAll() []NodeStatus {
	a.mu.RLock()
	nodes := make([]Node, len(a.nodes))
	copy(nodes, a.nodes)
	a.mu.RUnlock()

	results := make([]NodeStatus, len(nodes))
	var wg sync.WaitGroup

	for i, node := range nodes {
		wg.Add(1)
		go func(idx int, n Node) {
			defer wg.Done()
			results[idx] = a.fetchNode(n)
		}(i, node)
	}

	wg.Wait()
	return results
}

func (a *Aggregator) fetchNode(node Node) NodeStatus {
	status := NodeStatus{Node: node}

	// Fetch apps
	apps, err := a.fetchJSON(node.URL + "/api/apps")
	if err != nil {
		status.Error = err.Error()
		status.Node.Healthy = false
		return status
	}
	status.Apps = apps
	status.Node.Healthy = true
	status.Node.LastSeen = time.Now()

	// Fetch monitors
	monitors, _ := a.fetchJSON(node.URL + "/api/monitors")
	status.Monitors = monitors

	return status
}

func (a *Aggregator) fetchJSON(url string) ([]interface{}, error) {
	resp, err := a.client.Get(url)
	if err != nil {
		return nil, fmt.Errorf("unreachable: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}

	var result []interface{}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}
	return result, nil
}
