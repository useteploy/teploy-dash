package b2

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// fakeB2 is enough of the native API for the client's own tests.
type fakeB2 struct {
	mu      sync.Mutex
	caps    []string
	expiry  *int64
	buckets []map[string]any
	fail    map[string][2]any // call name -> {status, code}
	auth    string
	s3      string
	files   []map[string]string
	deleted []string
	bodies  map[string][]map[string]any
	echo    string
}

func newFake(t *testing.T) (*fakeB2, *Client) {
	t.Helper()
	f := &fakeB2{caps: []string{CapListBuckets, CapWriteBuckets, CapWriteKeys}, fail: map[string][2]any{},
		s3: "https://s3.us-west-004.backblazeb2.com", bodies: map[string][]map[string]any{}}
	srv := httptest.NewServer(http.HandlerFunc(f.serve(t)))
	t.Cleanup(srv.Close)
	f.auth = srv.URL
	return f, &Client{BaseURL: srv.URL, HTTP: srv.Client(), AnyHost: true}
}

func (f *fakeB2) serve(t *testing.T) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		name := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		if !strings.Contains(r.URL.Path, "/b2api/v4/") {
			t.Errorf("not the v4 API: %s", r.URL.Path)
		}
		writeJSON := func(status int, v any) {
			w.WriteHeader(status)
			json.NewEncoder(w).Encode(v)
		}
		if failure, ok := f.fail[name]; ok {
			writeJSON(failure[0].(int), map[string]any{"status": failure[0], "code": failure[1], "message": "the key " + f.echo + " was refused"})
			return
		}
		var body map[string]any
		if r.Method == http.MethodPost {
			json.NewDecoder(r.Body).Decode(&body)
			f.bodies[name] = append(f.bodies[name], body)
			if r.Header.Get("Authorization") != "TOKEN" {
				t.Errorf("%s without the token", name)
			}
		}
		switch name {
		case "b2_authorize_account":
			got := r.Header.Get("Authorization")
			if got != "Basic "+base64.StdEncoding.EncodeToString([]byte("KEYID:APPKEY")) {
				t.Errorf("authorization = %q", got)
			}
			writeJSON(200, map[string]any{"accountId": "acct", "authorizationToken": "TOKEN", "applicationKeyExpirationTimestamp": f.expiry,
				"apiInfo": map[string]any{"storageApi": map[string]any{"apiUrl": f.auth, "s3ApiUrl": f.s3,
					"allowed": map[string]any{"capabilities": f.caps, "buckets": nil, "namePrefix": nil}}}})
		case "b2_list_buckets":
			writeJSON(200, map[string]any{"buckets": f.buckets})
		case "b2_create_bucket":
			writeJSON(200, map[string]any{"bucketId": "new-id", "bucketName": body["bucketName"], "bucketType": body["bucketType"], "revision": 1})
		case "b2_update_bucket":
			writeJSON(200, map[string]any{"bucketId": body["bucketId"], "bucketName": "b", "bucketType": "allPrivate", "revision": 2})
		case "b2_create_key":
			writeJSON(200, map[string]any{"applicationKeyId": "K1", "applicationKey": "SECRET", "capabilities": body["capabilities"], "keyName": body["keyName"]})
		case "b2_delete_key":
			writeJSON(200, map[string]any{"applicationKeyId": body["applicationKeyId"]})
		case "b2_list_file_names":
			writeJSON(200, map[string]any{"files": []map[string]any{{"fileName": "teploy/"}}})
		case "b2_list_file_versions":
			if _, more := body["startFileName"]; more {
				writeJSON(200, map[string]any{"files": []map[string]string{{"fileName": "teploy/s/2", "fileId": "2"}}, "nextFileName": nil, "nextFileId": nil})
			} else {
				writeJSON(200, map[string]any{"files": f.files, "nextFileName": "teploy/s/2", "nextFileId": "2"})
			}
		case "b2_delete_file_version":
			f.deleted = append(f.deleted, body["fileId"].(string))
			writeJSON(200, map[string]any{})
		default:
			t.Errorf("unexpected call %s", name)
			w.WriteHeader(500)
		}
	}
}
