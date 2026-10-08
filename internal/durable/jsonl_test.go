package durable

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReadJSONLTailRepair(t *testing.T) {
	for _, tail := range []string{`{"id":"torn"`, `{"id":"complete"}`} {
		t.Run(tail, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "events.jsonl")
			prefix := "{\"id\":\"first\"}\n"
			if err := os.WriteFile(path, []byte(prefix+tail), 0600); err != nil {
				t.Fatal(err)
			}
			first, err := ReadJSONL(path)
			if err != nil {
				t.Fatal(err)
			}
			expected := prefix
			if tail == `{"id":"complete"}` {
				expected += tail + "\n"
			}
			if string(first) != expected {
				t.Fatalf("repair=%q", first)
			}
			f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
			if err != nil {
				t.Fatal(err)
			}
			f.WriteString("{\"id\":\"next\"}\n")
			f.Sync()
			f.Close()
			for i := 0; i < 2; i++ {
				data, err := ReadJSONL(path)
				if err != nil || string(data) != expected+"{\"id\":\"next\"}\n" {
					t.Fatalf("reopen=%q %v", data, err)
				}
			}
		})
	}
}

func TestReadJSONLInteriorDamageDoesNotRewrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	data := []byte("{\"ok\":true}\n{torn\n{\"next\":true}\n")
	os.WriteFile(path, data, 0600)
	if _, err := ReadJSONL(path); err == nil {
		t.Fatal("interior corruption accepted")
	}
	after, _ := os.ReadFile(path)
	if string(after) != string(data) {
		t.Fatal("interior corruption changed")
	}
}

func TestReadJSONLRepairFailureDoesNotAcknowledge(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permission fixture requires unprivileged process")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	data := []byte("{\"ok\":true}\n{torn")
	os.WriteFile(path, data, 0600)
	os.Chmod(dir, 0500)
	defer os.Chmod(dir, 0700)
	if _, err := ReadJSONL(path); err == nil {
		t.Fatal("failed repair acknowledged")
	}
	after, _ := os.ReadFile(path)
	if string(after) != string(data) {
		t.Fatal("failed repair changed source")
	}
}
