package b2

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func fmtAny(v any) string { return fmt.Sprint(v) }

func auth(t *testing.T, c *Client) *Session {
	t.Helper()
	s, err := c.Authorize(context.Background(), "KEYID", "APPKEY")
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestAuthorize_ReadsCapabilitiesEndpointAndExpiry(t *testing.T) {
	f, c := newFake(t)
	at := int64(1_900_000_000_000)
	f.expiry = &at
	s := auth(t, c)
	if s.Region != "us-west-004" || s.S3URL != "https://s3.us-west-004.backblazeb2.com" || s.AccountID != "acct" {
		t.Fatalf("session = %+v", s)
	}
	if s.ExpiresAt == nil || s.ExpiresAt.UnixMilli() != at {
		t.Fatalf("expiry = %v", s.ExpiresAt)
	}
	if m := s.Missing(); len(m) != 0 {
		t.Fatalf("missing = %v", m)
	}
	f.caps = []string{CapListBuckets}
	f.expiry = nil
	s = auth(t, c)
	if got := strings.Join(s.Missing(), ","); got != "writeBuckets,writeKeys" {
		t.Fatalf("missing = %s", got)
	}
	if s.ExpiresAt != nil {
		t.Fatal("no timestamp means it never expires")
	}
}

func TestAuthorize_ValidatesTheReply(t *testing.T) {
	f, c := newFake(t)
	for _, bad := range []string{"http://s3.us-west-004.backblazeb2.com", "https://s3.example.com", "https://evil.backblazeb2.com.example.net", "not a url"} {
		f.s3 = bad
		if _, err := c.Authorize(context.Background(), "KEYID", "APPKEY"); err == nil {
			t.Errorf("S3 address %q must be refused", bad)
		}
	}
	// The API address is where the token goes next.
	f.s3 = "https://s3.us-west-004.backblazeb2.com"
	strict := &Client{BaseURL: c.BaseURL, HTTP: c.HTTP}
	if _, err := strict.Authorize(context.Background(), "KEYID", "APPKEY"); err == nil || !strings.Contains(err.Error(), "outside backblazeb2.com") {
		t.Fatalf("a foreign API address must be refused: %v", err)
	}
}

func TestErrors_PlainAndNeverEchoing(t *testing.T) {
	f, c := newFake(t)
	f.echo = "APPKEY"
	cases := map[string]string{
		"transaction_cap_exceeded": "daily transaction cap",
		"email_not_verified":       "email address verified",
		"no_payment_history":       "payment method",
		"bad_auth_token":           "ended this session",
		"unauthorized":             "did not accept that key",
		"something_new":            "something_new",
	}
	for code, want := range cases {
		f.fail["b2_authorize_account"] = [2]any{403, code}
		_, err := c.Authorize(context.Background(), "KEYID", "APPKEY")
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v", code, err)
		}
		if strings.Contains(err.Error(), "APPKEY") {
			t.Errorf("%s: the message text leaked: %v", code, err)
		}
	}
	f.fail["b2_authorize_account"] = [2]any{403, "transaction_cap_exceeded"}
	_, err := c.Authorize(context.Background(), "KEYID", "APPKEY")
	if !IsCapped(err) {
		t.Fatal("IsCapped")
	}
}

func TestCreateKey_LimitsToBucketAndPrefix(t *testing.T) {
	f, c := newFake(t)
	s := auth(t, c)
	k, err := s.CreateKey(context.Background(), "n", []string{CapListFiles}, "bid", "teploy/prod/")
	if err != nil || k.ID != "K1" || k.Secret != "SECRET" {
		t.Fatalf("%+v %v", k, err)
	}
	body := f.bodies["b2_create_key"][0]
	ids, _ := body["bucketIds"].([]any)
	if len(ids) != 1 || ids[0] != "bid" || body["namePrefix"] != "teploy/prod/" || body["accountId"] != "acct" {
		t.Fatalf("body = %+v", body)
	}
}

func TestUpdateRules_SendsRevisionAndKeepsUnknownFields(t *testing.T) {
	f, c := newFake(t)
	s := auth(t, c)
	var rules Rules
	if err := json.Unmarshal([]byte(`[{"fileNamePrefix":"x/","daysFromHidingToDeleting":3,"daysFromUploadingToHiding":null,"novel":"kept"}]`), &rules); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateRules(context.Background(), "bid", rules, 7); err != nil {
		t.Fatal(err)
	}
	body := f.bodies["b2_update_bucket"][0]
	if body["ifRevisionIs"] != float64(7) {
		t.Fatalf("revision = %v", body["ifRevisionIs"])
	}
	sent := body["lifecycleRules"].([]any)[0].(map[string]any)
	if sent["novel"] != "kept" {
		t.Fatalf("an untouched rule must go back as it came: %+v", sent)
	}
}

func TestDeleteAllVersions_PagesAndRefusesAWholeBucket(t *testing.T) {
	f, c := newFake(t)
	s := auth(t, c)
	if _, err := s.DeleteAllVersions(context.Background(), "bid", ""); err == nil {
		t.Fatal("an empty prefix is a whole bucket")
	}
	f.files = []map[string]string{{"fileName": "teploy/s/1", "fileId": "1"}}
	n, err := s.DeleteAllVersions(context.Background(), "bid", "teploy/s/")
	if err != nil || n != 2 || strings.Join(f.deleted, ",") != "1,2" {
		t.Fatalf("n=%d err=%v deleted=%v", n, err, f.deleted)
	}
	f.files = []map[string]string{{"fileName": "elsewhere/1", "fileId": "9"}}
	if _, err := s.DeleteAllVersions(context.Background(), "bid", "teploy/s/"); err == nil {
		t.Fatal("a file outside the folder must stop the removal")
	}
}

func rule(t *testing.T, raw string) Rule {
	t.Helper()
	var r Rule
	if err := json.Unmarshal([]byte(raw), &r); err != nil {
		t.Fatal(err)
	}
	return r
}

func TestAnalyze(t *testing.T) {
	cases := []struct {
		name            string
		rule            string
		refused, warned bool
		s3              bool
	}{
		{"upload-to-hide on the base", `{"fileNamePrefix":"teploy/","daysFromUploadingToHiding":30}`, true, false, false},
		{"upload-to-hide on the whole bucket", `{"fileNamePrefix":"","daysFromUploadingToHiding":1}`, true, false, false},
		{"upload-to-hide inside the base", `{"fileNamePrefix":"teploy/prod/","daysFromUploadingToHiding":7}`, true, false, false},
		{"incoming is allowed", `{"fileNamePrefix":"teploy/prod/.incoming/","daysFromUploadingToHiding":1,"daysFromHidingToDeleting":1}`, false, false, false},
		{"elsewhere is not ours", `{"fileNamePrefix":"photos/","daysFromUploadingToHiding":1}`, false, false, false},
		{"shortens the window", `{"fileNamePrefix":"teploy/","daysFromHidingToDeleting":3}`, false, true, false},
		{"keep only last version", `{"fileNamePrefix":"","daysFromHidingToDeleting":1,"daysFromUploadingToHiding":null}`, false, true, false},
		{"longer is fine", `{"fileNamePrefix":"teploy/","daysFromHidingToDeleting":30}`, false, false, false},
		{"unknown field reads as S3-managed", `{"fileNamePrefix":"x/","id":"abc"}`, false, false, true},
	}
	for _, c := range cases {
		a := Analyze(Rules{rule(t, c.rule)}, "teploy", 14)
		if (len(a.Refusals) > 0) != c.refused || (len(a.Warnings) > 0) != c.warned || a.S3Managed != c.s3 {
			t.Errorf("%s: %+v", c.name, a)
		}
	}
	last := Analyze(Rules{rule(t, `{"fileNamePrefix":"","daysFromHidingToDeleting":1}`)}, "teploy", 14)
	if !strings.Contains(last.Warnings[0], "keeps only the last version") {
		t.Errorf("wording: %v", last.Warnings)
	}
}

func TestMerge_MatchesByPrefixAndKeepsOthers(t *testing.T) {
	other := rule(t, `{"fileNamePrefix":"photos/","daysFromHidingToDeleting":5,"novel":1}`)
	merged, changed, err := Merge(Rules{other}, "teploy", []string{"teploy/b", "teploy/a"}, 14, false)
	if err != nil || !changed || len(merged) != 4 {
		t.Fatalf("%+v %v %v", merged, changed, err)
	}
	raw, _ := json.Marshal(merged[0])
	if !strings.Contains(string(raw), `"novel"`) {
		t.Fatalf("a rule Dash does not own must survive: %s", raw)
	}
	base := merged[1]
	if base.FileNamePrefix != "teploy/" || *base.HideToDelete != 14 || *base.CancelUnfinishedLarge != 7 || base.UploadToHide != nil {
		t.Fatalf("base = %+v", base)
	}
	raw, _ = json.Marshal(base)
	if !strings.Contains(string(raw), `"daysFromUploadingToHiding":null`) {
		t.Fatalf("base must not hide on upload: %s", raw)
	}
	in := merged[2]
	if in.FileNamePrefix != "teploy/a/.incoming/" || *in.UploadToHide != 1 || *in.HideToDelete != 1 {
		t.Fatalf("incoming = %+v", in)
	}
	// Running it again changes nothing.
	if _, changed, err := Merge(merged, "teploy", []string{"teploy/a", "teploy/b"}, 14, false); err != nil || changed {
		t.Fatalf("a second merge must be a no-op: %v %v", changed, err)
	}
	// A window a person set stays, unless they ask for another.
	if again, changed, _ := Merge(merged, "teploy", nil, 30, false); changed || *again[1].HideToDelete != 14 {
		t.Fatal("an existing window must not be overwritten silently")
	}
	if again, changed, _ := Merge(merged, "teploy", nil, 30, true); !changed || *again[1].HideToDelete != 30 {
		t.Fatal("an explicit window replaces it")
	}
}

func TestMerge_RespectsTheRuleLimit(t *testing.T) {
	var many Rules
	for i := 0; i < MaxRules; i++ {
		many = append(many, Rule{FileNamePrefix: "x" + strings.Repeat("y", i) + "/"})
	}
	if _, _, err := Merge(many, "teploy", nil, 14, false); err == nil {
		t.Fatal("over the limit must be refused")
	}
}

func TestMerge_AnEmptyBaseNeverMakesAWholeBucketRule(t *testing.T) {
	merged, changed, err := Merge(nil, "", []string{"prod", "stage"}, 14, false)
	if err != nil || !changed {
		t.Fatalf("%v %v", changed, err)
	}
	seen := map[string]bool{}
	for _, r := range merged {
		seen[r.FileNamePrefix] = true
	}
	if seen[""] || !seen["prod/"] || !seen["stage/"] || !seen["prod/.incoming/"] || !seen["stage/.incoming/"] {
		t.Fatalf("%+v", seen)
	}
	if got := EffectiveWindow(merged, "", []string{"prod", "stage"}, 99); got != 14 {
		t.Fatalf("window = %d", got)
	}
	if text := strings.Join(RulesText("", []string{"prod"}, 14), " "); strings.Contains(text, "whole bucket") || !strings.Contains(text, "On prod/:") {
		t.Fatalf("%s", text)
	}
}

func TestEffectiveWindow_AnExistingRuleWins(t *testing.T) {
	existing := Rules{rule(t, `{"fileNamePrefix":"teploy/","daysFromHidingToDeleting":30}`)}
	merged, _, _ := Merge(existing, "teploy", nil, 14, false)
	if got := EffectiveWindow(merged, "teploy", nil, 14); got != 30 {
		t.Fatalf("%d", got)
	}
}

func TestRevision_ReadsANumberOrAString(t *testing.T) {
	for raw, want := range map[string]Revision{`{"revision":7}`: 7, `{"revision":"7"}`: 7, `{}`: 0} {
		var b Bucket
		if err := json.Unmarshal([]byte(raw), &b); err != nil || b.Revision != want {
			t.Errorf("%s: %d %v", raw, b.Revision, err)
		}
	}
	var b Bucket
	if err := json.Unmarshal([]byte(`{"revision":"x"}`), &b); err == nil {
		t.Fatal("not a number")
	}
}

func TestAuthorize_WrongKeyAndNotAllowedReadDifferently(t *testing.T) {
	f, c := newFake(t)
	f.fail["b2_authorize_account"] = [2]any{401, "unauthorized"}
	_, err := c.Authorize(context.Background(), "KEYID", "APPKEY")
	if err == nil || !strings.Contains(err.Error(), "did not accept") {
		t.Fatalf("%v", err)
	}
	delete(f.fail, "b2_authorize_account")
	s := auth(t, c)
	f.fail["b2_list_buckets"] = [2]any{401, "unauthorized"}
	if _, err := s.ListBuckets(context.Background()); err == nil || !strings.Contains(err.Error(), "not allowed") {
		t.Fatalf("%v", err)
	}
}

func TestSend_RetriesA503ButNothingElse(t *testing.T) {
	f, c := newFake(t)
	c.RetryWait = 1
	s := auth(t, c)
	f.fail["b2_create_key"] = [2]any{503, "service_unavailable"}
	f.failOnce = map[string]int{"b2_create_key": 1}
	if _, err := s.CreateKey(context.Background(), "n", []string{CapListFiles}, "b", "p/"); err != nil {
		t.Fatalf("a 503 is retried: %v", err)
	}
	// One that keeps failing stops, with the status.
	f.fail["b2_create_key"] = [2]any{503, "service_unavailable"}
	f.failOnce = map[string]int{}
	if _, err := s.CreateKey(context.Background(), "n", []string{CapListFiles}, "b", "p/"); err == nil || !strings.Contains(err.Error(), "503") {
		t.Fatalf("%v", err)
	}
	if f.attempts["b2_create_key"] != 1+1+3 {
		t.Fatalf("attempts = %d", f.attempts["b2_create_key"])
	}
	// A 500 might have landed, so it is not.
	f.attempts = map[string]int{}
	f.fail["b2_create_key"] = [2]any{500, "internal_error"}
	if _, err := s.CreateKey(context.Background(), "n", []string{CapListFiles}, "b", "p/"); err == nil || f.attempts["b2_create_key"] != 1 {
		t.Fatalf("%v attempts=%d", err, f.attempts["b2_create_key"])
	}
}

func TestIsMissingKey_ReadsTheMessageWithoutEchoingIt(t *testing.T) {
	f, c := newFake(t)
	s := auth(t, c)
	f.fail["b2_delete_key"] = [2]any{400, "bad_request"}
	f.echo = "APPKEY"
	err := s.DeleteKey(context.Background(), "K")
	if IsMissingKey(err) || strings.Contains(err.Error(), "APPKEY") {
		t.Fatalf("%v", err)
	}
	f.message = "application key does not exist"
	err = s.DeleteKey(context.Background(), "K")
	if !IsMissingKey(err) || strings.Contains(err.Error(), "exist") {
		t.Fatalf("%v", err)
	}
}

func TestListBuckets_AsksForSnapshotBucketsToo(t *testing.T) {
	f, c := newFake(t)
	s := auth(t, c)
	f.buckets = []map[string]any{{"bucketId": "1", "bucketName": "s", "bucketType": BucketSnapshot, "revision": "3"}}
	got, err := s.ListBuckets(context.Background())
	if err != nil || got[0].Type != BucketSnapshot || got[0].Revision != 3 {
		t.Fatalf("%+v %v", got, err)
	}
	if !strings.Contains(fmtAny(f.bodies["b2_list_buckets"][0]["bucketTypes"]), BucketSnapshot) {
		t.Fatalf("%+v", f.bodies["b2_list_buckets"][0])
	}
}

func TestRules_ReadArrayObjectAndNull(t *testing.T) {
	for raw, want := range map[string]int{`[]`: 0, `null`: 0, `{"fileNamePrefix":"a/"}`: 1, `[{"fileNamePrefix":"a/"},{"fileNamePrefix":"b/"}]`: 2} {
		var rs Rules
		if err := json.Unmarshal([]byte(raw), &rs); err != nil || len(rs) != want {
			t.Errorf("%s: %d %v", raw, len(rs), err)
		}
	}
}
