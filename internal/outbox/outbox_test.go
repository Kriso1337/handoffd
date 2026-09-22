package outbox

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

var now = time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)

func reply(id string) Record {
	return Record{ID: id, At: now, ThreadTS: "1789000000.000001", Kind: KindReply, Text: "hello", SessionID: "sess-1"}
}

func TestWriteThenPendingReturnsRecordsWithoutReceiptsInOrder(t *testing.T) {
	d := New(filepath.Join(t.TempDir(), "outbox"))
	later := reply("b")
	later.At = now.Add(time.Minute)
	if _, err := d.Write(later); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if _, err := d.Write(reply("a")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	done := reply("c")
	if _, err := d.Write(done); err != nil {
		t.Fatal(err)
	}
	if err := d.WriteReceipt(Receipt{ID: "c", Status: StatusPosted, At: now}); err != nil {
		t.Fatal(err)
	}

	records, malformed, err := d.Pending()

	if err != nil || len(malformed) != 0 {
		t.Fatalf("Pending: %v %v", err, malformed)
	}
	if len(records) != 2 || records[0].ID != "a" || records[1].ID != "b" {
		t.Errorf("records = %+v, want a then b by time, c already settled", records)
	}
	if _, err := os.Stat(filepath.Join(d.Path(), "a.json.tmp")); !os.IsNotExist(err) {
		t.Error("temporary file must not survive an atomic write")
	}
}

func TestWriteRejectsIncompleteRecords(t *testing.T) {
	d := New(t.TempDir())
	cases := map[string]Record{
		"no thread":        {ID: "x", Kind: KindReply, Text: "t", SessionID: "s"},
		"no session":       {ID: "x", ThreadTS: "1.1", Kind: KindReply, Text: "t"},
		"empty reply":      {ID: "x", ThreadTS: "1.1", Kind: KindReply, Text: "  ", SessionID: "s"},
		"note sans sha":    {ID: "x", ThreadTS: "1.1", Kind: KindMRNote, Text: "t", SessionID: "s"},
		"bad phase":        {ID: "x", ThreadTS: "1.1", Kind: KindMarker, Phase: "done", SHA: "abc1234", SessionID: "s"},
		"unknown kind":     {ID: "x", ThreadTS: "1.1", Kind: "tweet", Text: "t", SessionID: "s"},
		"done without sha": {ID: "x", ThreadTS: "1.1", Kind: KindReviewDone, SessionID: "s"},
	}
	for name, r := range cases {
		if _, err := d.Write(r); err == nil {
			t.Errorf("%s: Write accepted %+v", name, r)
		}
	}
	ok := Record{ID: "y", ThreadTS: "1.1", Kind: KindReviewDone, SHA: "abc1234", Blockers: 1, SessionID: "s"}
	if _, err := d.Write(ok); err != nil {
		t.Errorf("valid review_done rejected: %v", err)
	}
}

func TestMalformedFilesAreReportedAndQuarantined(t *testing.T) {
	d := New(t.TempDir())
	broken := filepath.Join(d.Path(), "zz.json")
	if err := os.WriteFile(broken, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Write(reply("a")); err != nil {
		t.Fatal(err)
	}

	records, malformed, err := d.Pending()
	if err != nil || len(records) != 1 || len(malformed) != 1 || malformed[0].Path != broken {
		t.Fatalf("Pending = %+v %+v %v", records, malformed, err)
	}
	if err := d.Quarantine(broken, "malformed: "+malformed[0].Err.Error(), now); err != nil {
		t.Fatalf("Quarantine: %v", err)
	}

	if _, err := os.Stat(filepath.Join(d.Path(), "rejected", "zz.json")); err != nil {
		t.Errorf("file must move to rejected/: %v", err)
	}
	rc, ok, err := d.ReadReceipt("zz")
	if err != nil || !ok || rc.Status != StatusRejected || rc.Reason == "" {
		t.Errorf("receipt = %+v %v %v", rc, ok, err)
	}
	if _, malformed, _ := d.Pending(); len(malformed) != 0 {
		t.Errorf("quarantined file must not be reported again: %+v", malformed)
	}
}

func TestRetryDropsOnlyPendingReceipts(t *testing.T) {
	d := New(t.TempDir())
	for _, id := range []string{"p", "q", "r"} {
		if _, err := d.Write(reply(id)); err != nil {
			t.Fatal(err)
		}
	}
	_ = d.WriteReceipt(Receipt{ID: "p", Status: StatusPending, Reason: "slack down", At: now})
	_ = d.WriteReceipt(Receipt{ID: "q", Status: StatusPosted, At: now})

	n, err := d.Retry()

	if err != nil || n != 1 {
		t.Fatalf("Retry = %d, %v", n, err)
	}
	records, _, _ := d.Pending()
	if len(records) != 2 || records[0].ID != "p" || records[1].ID != "r" {
		t.Errorf("pending after retry = %+v", records)
	}
}

func TestSweepRemovesOldFilesOnly(t *testing.T) {
	d := New(t.TempDir())
	if _, err := d.Write(reply("old")); err != nil {
		t.Fatal(err)
	}
	_ = d.WriteReceipt(Receipt{ID: "old", Status: StatusPosted, At: now})
	if _, err := d.Write(reply("fresh")); err != nil {
		t.Fatal(err)
	}
	stale := time.Now().Add(-48 * time.Hour)
	for _, name := range []string{"old.json", "old.receipt.json"} {
		if err := os.Chtimes(filepath.Join(d.Path(), name), stale, stale); err != nil {
			t.Fatal(err)
		}
	}

	removed, err := d.Sweep(time.Now(), 24*time.Hour)

	if err != nil || removed != 2 {
		t.Fatalf("Sweep = %d, %v", removed, err)
	}
	if _, ok, _ := d.ReadReceipt("old"); ok {
		t.Error("old receipt must be gone")
	}
	if records, _, _ := d.Pending(); len(records) != 1 || records[0].ID != "fresh" {
		t.Errorf("pending = %+v", records)
	}
}

func TestWaitReturnsReceiptOrTimesOut(t *testing.T) {
	d := New(t.TempDir())
	go func() {
		time.Sleep(5 * time.Millisecond)
		_ = d.WriteReceipt(Receipt{ID: "w", Status: StatusPosted, Permalink: "https://x/p1", At: now})
	}()

	rc, ok, err := Wait(context.Background(), d, "w", time.Second, time.Millisecond)
	if err != nil || !ok || rc.Permalink != "https://x/p1" {
		t.Errorf("Wait = %+v %v %v", rc, ok, err)
	}

	_, ok, err = Wait(context.Background(), d, "absent", 5*time.Millisecond, time.Millisecond)
	if err != nil || ok {
		t.Errorf("Wait on a missing receipt = %v %v, want a clean timeout", ok, err)
	}
}

func TestMissingDirectoryIsEmpty(t *testing.T) {
	d := New(filepath.Join(t.TempDir(), "absent"))
	records, malformed, err := d.Pending()
	if err != nil || len(records) != 0 || len(malformed) != 0 {
		t.Errorf("Pending = %v %v %v", records, malformed, err)
	}
	if n, err := d.Retry(); err != nil || n != 0 {
		t.Errorf("Retry = %d %v", n, err)
	}
	if n, err := d.Sweep(now, time.Hour); err != nil || n != 0 {
		t.Errorf("Sweep = %d %v", n, err)
	}
}
