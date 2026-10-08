package store

import (
	"os"
	"testing"
	"time"
)

// The caller supplies an isolated pgwire database. This exercises real SQL
// migration/upsert/CAS, while ordinary hermetic suites use FileStore.
func TestPgwireRestoreIncarnationAndPersistence(t *testing.T) {
	url := os.Getenv("TEPLOY_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEPLOY_TEST_DATABASE_URL is required for isolated pgwire acceptance")
	}
	s, err := NewNucleusStore(url)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	id := "restore-fixture"
	defer s.DeleteRestoreTest(id)
	config := RestoreTest{ID: id, Server: "prod", App: "web", Accessory: "db", Bucket: "b", Region: "us-east-1", IntervalHours: 24, Enabled: true}
	if err := s.SaveRestoreTest(config); err != nil {
		t.Fatal(err)
	}
	first, err := s.GetRestoreTest(id)
	if err != nil {
		t.Fatal(err)
	}
	first.LastRunAt = time.Now()
	first.LastOK = true
	first.LastDetail = "verified"
	if applied, err := s.SaveRestoreTestResult(id, *first); err != nil || !applied {
		t.Fatalf("current result: %v %v", applied, err)
	}
	config.IntervalHours = 12
	if err := s.SaveRestoreTest(config); err != nil {
		t.Fatal(err)
	}
	edited, err := s.GetRestoreTest(id)
	if err != nil {
		t.Fatal(err)
	}
	if edited.Incarnation != first.Incarnation || !edited.LastOK || edited.LastRunAt.IsZero() || edited.IntervalHours != 12 {
		t.Fatalf("config edit lost verdict/incarnation: %+v", edited)
	}
	all, err := s.ListRestoreTests()
	if err != nil || len(all) == 0 {
		t.Fatalf("list: %v %v", all, err)
	}
	other := config
	other.Bucket = "other"
	if err := s.SaveRestoreTest(other); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveRestoreTest(config); err != nil {
		t.Fatal(err)
	}
	if applied, err := s.SaveRestoreTestResult(id, *first); err != nil || applied {
		t.Fatalf("ABA applied: %v %v", applied, err)
	}
	before, _ := s.GetRestoreTest(id)
	if err := s.DeleteRestoreTest(id); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveRestoreTest(config); err != nil {
		t.Fatal(err)
	}
	if applied, err := s.SaveRestoreTestResult(id, *before); err != nil || applied {
		t.Fatalf("recreated applied: %v %v", applied, err)
	}
	after, err := s.GetRestoreTest(id)
	if err != nil || after.LastOK || !after.LastRunAt.IsZero() {
		t.Fatalf("recreation verdict: %+v %v", after, err)
	}
}
