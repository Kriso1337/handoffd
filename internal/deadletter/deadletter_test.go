package deadletter

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/Kriso1337/handoffd/internal/notifylog"
)

var now = time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)

func note(ts string) notifylog.Notification {
	return notifylog.Notification{ID: "T1_" + ts, TeamID: "T1", Channel: "C1", MsgTS: ts, ThreadTS: "1.0"}
}

func TestRemoveDropsOnlyTheNamedRecord(t *testing.T) {
	path := filepath.Join(t.TempDir(), "deadletter.jsonl")
	q := New(path)
	if err := q.Add(note("1.1"), "no triage verdict", now); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := q.Add(note("1.2"), "launch rate limit", now.Add(time.Minute)); err != nil {
		t.Fatalf("Add: %v", err)
	}

	if err := q.Remove("T1_1.1"); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	got, err := q.Peek()
	if err != nil {
		t.Fatalf("Peek: %v", err)
	}
	if len(got) != 1 || got[0].Notification.MsgTS != "1.2" || got[0].Reason != "launch rate limit" {
		t.Errorf("records = %+v", got)
	}
	if err := q.Remove("T1_absent"); err != nil {
		t.Errorf("removing an unknown id must be a no-op, got %v", err)
	}
	if again, _ := q.Peek(); len(again) != 1 {
		t.Errorf("records after no-op remove = %+v", again)
	}
}

func TestPeekLeavesRecordsInPlace(t *testing.T) {
	path := filepath.Join(t.TempDir(), "deadletter.jsonl")
	q := New(path)
	if err := q.Add(note("1.1"), "error", now); err != nil {
		t.Fatal(err)
	}

	first, err := q.Peek()
	if err != nil {
		t.Fatalf("Peek: %v", err)
	}
	second, _ := q.Peek()

	if len(first) != 1 || len(second) != 1 {
		t.Errorf("peek must not consume: %d then %d", len(first), len(second))
	}
}

func TestAddDeduplicatesByNotificationID(t *testing.T) {
	path := filepath.Join(t.TempDir(), "deadletter.jsonl")
	q := New(path)
	_ = q.Add(note("1.1"), "first", now)
	_ = q.Add(note("1.1"), "second", now.Add(time.Minute))

	got, _ := q.Peek()

	if len(got) != 1 || got[0].Reason != "second" {
		t.Errorf("records = %+v, want the latest reason once", got)
	}
}

func TestMissingFileIsEmpty(t *testing.T) {
	got, err := New(filepath.Join(t.TempDir(), "absent.jsonl")).Peek()
	if err != nil || len(got) != 0 {
		t.Errorf("got %v, %v", got, err)
	}
}
