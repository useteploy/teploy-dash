// Package audit is Dash's append-only record of configuration actions
// (plan principle 8: every new action has a capability AND an audit entry).
// File store only: <dir>/audit.jsonl, bounded by compaction to the newest
// entries.
package audit

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/useteploy/teploy-dash/internal/durable"
)

const (
	fileName   = "audit.jsonl"
	maxLines   = 4000
	keepOnTrim = 2000
)

// Entry is one audited action.
type Entry struct {
	At     time.Time `json:"at"`
	Actor  string    `json:"actor"`
	Action string    `json:"action"`
	Target string    `json:"target,omitempty"`
	Detail string    `json:"detail,omitempty"`
}

// Log is the audit journal.
type Log struct {
	path string
	mu   sync.Mutex
	// entries is the retained window, oldest first.
	entries []Entry
}

// Open loads (or initializes) the journal in dir. dir "" keeps entries in
// memory only.
func Open(dir string) (*Log, error) {
	l := &Log{}
	if dir == "" {
		return l, nil
	}
	l.path = filepath.Join(dir, fileName)
	data, err := durable.ReadJSONL(l.path)
	if errors.Is(err, os.ErrNotExist) {
		return l, nil
	}
	if err != nil {
		return nil, err
	}
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		var e Entry
		if json.Unmarshal(sc.Bytes(), &e) == nil {
			l.entries = append(l.entries, e)
		}
	}
	return l, sc.Err()
}

// Append records an action. A write failure is logged loudly; the action
// it describes has already happened and is not undone.
func (l *Log) Append(e Entry) {
	if e.At.IsZero() {
		e.At = time.Now().UTC()
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.entries = append(l.entries, e)
	if l.path == "" {
		if len(l.entries) > maxLines {
			l.entries = append([]Entry(nil), l.entries[len(l.entries)-keepOnTrim:]...)
		}
		return
	}
	if len(l.entries) > maxLines {
		l.entries = append([]Entry(nil), l.entries[len(l.entries)-keepOnTrim:]...)
		var buf []byte
		for _, x := range l.entries {
			line, _ := json.Marshal(x)
			buf = append(append(buf, line...), '\n')
		}
		if err := durable.Replace(l.path, buf, 0600); err != nil {
			log.Printf("[audit] compaction failed: %v", err)
		}
		return
	}
	line, err := json.Marshal(e)
	if err == nil {
		var f *os.File
		f, err = os.OpenFile(l.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
		if err == nil {
			_, err = f.Write(append(line, '\n'))
			if cerr := f.Close(); err == nil {
				err = cerr
			}
		}
	}
	if err != nil {
		log.Printf("[audit] APPEND FAILED for %s %s: %v", e.Action, e.Target, err)
	}
}

// Recent returns up to n entries, newest first.
func (l *Log) Recent(n int) []Entry {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]Entry, 0, n)
	for i := len(l.entries) - 1; i >= 0 && (n <= 0 || len(out) < n); i-- {
		out = append(out, l.entries[i])
	}
	return out
}
