package s3probe

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// The GET example from AWS's "Signature Calculations for the Authorization
// Header" (S3 API reference): a published vector, so the signer is checked
// against something other than itself.
func TestSignMatchesAWSPublishedVector(t *testing.T) {
	req, err := http.NewRequest(http.MethodGet, "https://examplebucket.s3.amazonaws.com/test.txt", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Range", "bytes=0-9")
	sign(req, nil, "AKIAIOSFODNN7EXAMPLE", "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY", "us-east-1",
		time.Date(2013, 5, 24, 0, 0, 0, 0, time.UTC))
	want := "AWS4-HMAC-SHA256 Credential=AKIAIOSFODNN7EXAMPLE/20130524/us-east-1/s3/aws4_request, " +
		"SignedHeaders=host;range;x-amz-content-sha256;x-amz-date, " +
		"Signature=f0e8bdb87c964420e857bd35b5d6ed310bd44f0170aba48dd91039c6036bdb41"
	if got := req.Header.Get("Authorization"); got != want {
		t.Fatalf("Authorization =\n %s\nwant\n %s", got, want)
	}
}

// fakeStore is a one-object S3: deleteStatus decides the delete's answer.
type fakeStore struct {
	mu           sync.Mutex
	objects      map[string][]byte
	writeStatus  int
	readStatus   int
	readCorrupt  bool
	deleteStatus int
	// keepAfterDelete: an accepted delete leaves the object readable.
	keepAfterDelete bool
	methods         []string
	auth            []string
}

func (f *fakeStore) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.methods = append(f.methods, r.Method)
	f.auth = append(f.auth, r.Header.Get("Authorization"))
	fail := func(status int) {
		w.WriteHeader(status)
		io.WriteString(w, `<Error><Code>AccessDenied</Code><Message>key AKIDECHOED denied</Message></Error>`)
	}
	switch r.Method {
	case http.MethodPut:
		if f.writeStatus != 0 {
			fail(f.writeStatus)
			return
		}
		body, _ := io.ReadAll(r.Body)
		f.objects[r.URL.Path] = body
	case http.MethodGet:
		if f.readStatus != 0 {
			fail(f.readStatus)
			return
		}
		body, ok := f.objects[r.URL.Path]
		if !ok && !f.keepAfterDelete {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if f.readCorrupt {
			body = []byte("different")
		}
		w.Write(body)
	case http.MethodDelete:
		if f.deleteStatus != 0 {
			fail(f.deleteStatus)
			return
		}
		if !f.keepAfterDelete {
			delete(f.objects, r.URL.Path)
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

func runAgainst(t *testing.T, f *fakeStore) Result {
	t.Helper()
	f.objects = map[string][]byte{}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return Run(context.Background(), Config{Endpoint: srv.URL, Region: "auto", Bucket: "b", AccessKey: "AK", SecretKey: "SECRETVALUE", Key: "prod/.incoming/dash-probe-x"})
}

func TestRunWritesReadsDeletes(t *testing.T) {
	f := &fakeStore{}
	res := runAgainst(t, f)
	if !res.Write || !res.Read || res.Delete != DeleteRemoved || res.Error != "" {
		t.Fatalf("res = %+v", res)
	}
	if strings.Join(f.methods, ",") != "PUT,GET,DELETE,GET" {
		t.Fatalf("methods = %v", f.methods)
	}
	if !res.Hidden {
		t.Fatalf("the object is gone, so it reads as hidden: %+v", res)
	}
	if len(f.objects) != 0 {
		t.Fatalf("object left behind: %v", f.objects)
	}
	for _, a := range f.auth {
		if strings.Contains(a, "SECRETVALUE") || !strings.Contains(a, "Credential=AK/") {
			t.Fatalf("authorization header = %q", a)
		}
	}
}

func TestRunReportsARefusedDelete(t *testing.T) {
	res := runAgainst(t, &fakeStore{deleteStatus: http.StatusForbidden})
	if !res.Write || !res.Read || res.Delete != DeleteRefused || res.Hidden {
		t.Fatalf("res = %+v", res)
	}
}

// An accepted delete that leaves the object readable did not hide anything.
func TestRunSeesAnObjectStillReadable(t *testing.T) {
	res := runAgainst(t, &fakeStore{keepAfterDelete: true})
	if res.Delete != DeleteRemoved || res.Hidden {
		t.Fatalf("res = %+v", res)
	}
}

func TestRunStopsAtAFailedWriteAndNeverEchoesTheBody(t *testing.T) {
	f := &fakeStore{writeStatus: http.StatusForbidden}
	res := runAgainst(t, f)
	if res.Write || res.Read || res.Delete != DeleteNotReached {
		t.Fatalf("res = %+v", res)
	}
	if !strings.Contains(res.Error, "AccessDenied") || strings.Contains(res.Error, "AKIDECHOED") {
		t.Fatalf("error = %q", res.Error)
	}
	if len(f.methods) != 1 {
		t.Fatalf("methods = %v", f.methods)
	}
}

func TestRunCatchesACorruptRead(t *testing.T) {
	res := runAgainst(t, &fakeStore{readCorrupt: true})
	if res.Read || !strings.Contains(res.Error, "different") {
		t.Fatalf("res = %+v", res)
	}
	if res.Delete != DeleteRemoved {
		t.Fatalf("the object must still be removed: %+v", res)
	}
}

func TestRunDoesNotFollowRedirects(t *testing.T) {
	hit := false
	other := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hit = true }))
	defer other.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/steal", http.StatusTemporaryRedirect)
	}))
	defer srv.Close()
	res := Run(context.Background(), Config{Endpoint: srv.URL, Region: "auto", Bucket: "b", AccessKey: "AK", SecretKey: "S", Key: "k"})
	if res.Write || hit {
		t.Fatalf("res = %+v hit = %v", res, hit)
	}
}

func TestRunRejectsANonHTTPEndpoint(t *testing.T) {
	res := Run(context.Background(), Config{Endpoint: "file:///etc/passwd", Region: "auto", Bucket: "b", AccessKey: "AK", SecretKey: "S", Key: "k"})
	if res.Write || !strings.Contains(res.Error, "http(s)") {
		t.Fatalf("res = %+v", res)
	}
}
