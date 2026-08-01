package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestWriteRawJSON_ValidJSONIsWrapped(t *testing.T) {
	w := httptest.NewRecorder()
	writeRawJSON(w, `{"ok":true,"count":3}`)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
	got := w.Body.String()
	want := `{"data":{"count":3,"ok":true}}` + "\n"
	if got != want {
		t.Errorf("body = %q, want %q", got, want)
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
