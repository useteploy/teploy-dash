package audit

import (
	"testing"
)

func TestAppendPersistsAndRecentIsNewestFirst(t *testing.T) {
	dir := t.TempDir()
	l, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	l.Append(Entry{Actor: "tyler", Action: "channel.create", Target: "phone"})
	l.Append(Entry{Actor: "tyler", Action: "silence.create", Target: "sil_1"})
	l2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	got := l2.Recent(10)
	if len(got) != 2 || got[0].Action != "silence.create" || got[1].Target != "phone" || got[0].At.IsZero() {
		t.Fatalf("recent = %+v", got)
	}
}

func TestCompactionBoundsTheJournal(t *testing.T) {
	dir := t.TempDir()
	l, _ := Open(dir)
	for i := 0; i < maxLines+1; i++ {
		l.Append(Entry{Action: "x"})
	}
	l2, _ := Open(dir)
	if n := len(l2.Recent(0)); n != keepOnTrim {
		t.Fatalf("retained %d, want %d", n, keepOnTrim)
	}
}
