package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestWriteRawJSON_ValidJSONIsWrapped(t *testing.T) {
	w := httptest.NewRecorder()
	writeRawJSON(w, `{"ok":true,"count":3}`)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
	// A35: the exact bytes are forwarded (key order preserved, integers
	// never round-tripped through float64) — the old decode/re-encode path
	// corrupted integers above 2^53.
	got := w.Body.String()
	want := `{"data":{"ok":true,"count":3}}` + "\n"
	if got != want {
		t.Errorf("body = %q, want %q", got, want)
	}
}

// A35: a large integer survives the passthrough exactly. Decoding into
// interface{} re-encoded 9007199254740993 as 9007199254740992.
func TestWriteRawJSON_LargeIntegersSurvive(t *testing.T) {
	w := httptest.NewRecorder()
	writeRawJSON(w, `{"id":9007199254740993,"neg":-9007199254740993}`)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "9007199254740993") || strings.Contains(w.Body.String(), "9007199254740992") {
		t.Errorf("large integer corrupted: %q", w.Body.String())
	}
}

func TestWriteRawJSON_EmptyIsNullData(t *testing.T) {
	w := httptest.NewRecorder()
	writeRawJSON(w, "   ")

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
	if w.Body.String() != `{"data":null}`+"\n" {
		t.Errorf("body = %q", w.Body.String())
	}
}

// DASH-007: malformed CLI output used to be raw-concatenated into the
// response body ({"data":<raw>}), which itself produced invalid JSON and
// let callers checking only HTTP status treat a broken delegate call as
// success. It must now fail as a typed error, not pass the raw text through.
func TestWriteRawJSON_MalformedOutputIsTypedError(t *testing.T) {
	cases := []string{
		"not json at all",
		`{"unterminated": `,
		`{"a":1}{"b":2}`,
		`plain text with "quotes" and \backslashes and` + "\nnewlines",
	}
	for _, raw := range cases {
		w := httptest.NewRecorder()
		writeRawJSON(w, raw)

		if w.Code != http.StatusBadGateway {
			t.Errorf("raw %q: expected 502, got %d (body %q)", raw, w.Code, w.Body.String())
		}
		var parsed map[string]string
		if err := json.Unmarshal(w.Body.Bytes(), &parsed); err != nil {
			t.Errorf("raw %q: response body is not valid JSON: %v (body %q)", raw, err, w.Body.String())
			continue
		}
		if parsed["error"] == "" {
			t.Errorf("raw %q: expected an error field, got %q", raw, w.Body.String())
		}
	}
}
