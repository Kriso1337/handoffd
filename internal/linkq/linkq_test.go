package linkq

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

var now = time.Date(2026, 9, 15, 17, 18, 0, 0, time.UTC)

func request(id string) Request {
	return Request{
		ID: id, At: now, ThreadTS: "1789480920.729569", Kind: "other",
		Channel: "C0000000002", Label: "OPS", WindowID: "@115",
	}
}

func TestWriteThenPendingReturnsRequestsWithoutReceiptsInOrder(t *testing.T) {
	d := New(filepath.Join(t.TempDir(), "links"))
	later := request("b")
	later.At = now.Add(time.Minute)
	if _, err := d.Write(later); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if _, err := d.Write(request("a")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if _, err := d.Write(request("c")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := d.WriteReceipt(Receipt{ID: "c", Status: StatusLinked, At: now}); err != nil {
		t.Fatalf("WriteReceipt: %v", err)
	}

	requests, malformed, err := d.Pending()

	if err != nil || len(malformed) != 0 {
		t.Fatalf("Pending: %v %v", err, malformed)
	}
	if len(requests) != 2 || requests[0].ID != "a" || requests[1].ID != "b" {
		t.Errorf("requests = %+v, want a then b by time, c already settled", requests)
	}
	if _, err := os.Stat(filepath.Join(d.Path(), "a.json.tmp")); !os.IsNotExist(err) {
		t.Error("temporary file must not survive an atomic write")
	}
}

func TestWriteRejectsIncompleteRequests(t *testing.T) {
	d := New(t.TempDir())
	cases := map[string]Request{
		"no id":        {At: now, ThreadTS: "1.1", Kind: "other", WindowID: "@1"},
		"no thread":    {ID: "x", At: now, Kind: "other", WindowID: "@1"},
		"no window":    {ID: "x", At: now, ThreadTS: "1.1", Kind: "other"},
		"unknown kind": {ID: "x", At: now, ThreadTS: "1.1", Kind: "triage", WindowID: "@1"},
	}
	for name, r := range cases {
		if _, err := d.Write(r); err == nil {
			t.Errorf("%s: Write accepted %+v", name, r)
		}
	}
	for _, kind := range []string{"review", "help", "other"} {
		ok := request("y-" + kind)
		ok.Kind = kind
		if _, err := d.Write(ok); err != nil {
			t.Errorf("valid %s request rejected: %v", kind, err)
		}
	}
}

func TestMalformedFilesAreReportedAndQuarantined(t *testing.T) {
	d := New(t.TempDir())
	broken := filepath.Join(d.Path(), "zz.json")
	if err := os.MkdirAll(d.Path(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(broken, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Write(request("a")); err != nil {
		t.Fatal(err)
	}

	requests, malformed, err := d.Pending()
	if err != nil || len(requests) != 1 || len(malformed) != 1 || malformed[0].Path != broken {
		t.Fatalf("Pending = %+v %+v %v", requests, malformed, err)
	}

	if err := d.Quarantine(broken, "malformed: "+malformed[0].Err.Error(), now); err != nil {
		t.Fatalf("Quarantine: %v", err)
	}
	if _, err := os.Stat(filepath.Join(d.Path(), "rejected", "zz.json")); err != nil {
		t.Errorf("file must move to rejected/: %v", err)
	}
	rc, ok, err := d.ReadReceipt("zz")
	if err != nil || !ok || rc.Status != StatusRejected {
		t.Errorf("quarantine must leave a rejected receipt, got %+v %v %v", rc, ok, err)
	}
}

func TestWaitReturnsTheReceiptWrittenWhileItWaits(t *testing.T) {
	d := New(t.TempDir())
	if _, err := d.Write(request("a")); err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(20 * time.Millisecond)
		_ = d.WriteReceipt(Receipt{ID: "a", Status: StatusLinked, WindowName: "sky/OPS-1718", At: now})
	}()

	rc, ok, err := Wait(context.Background(), d, "a", time.Second, 5*time.Millisecond)

	if err != nil || !ok {
		t.Fatalf("Wait = (%+v, %v, %v)", rc, ok, err)
	}
	if rc.WindowName != "sky/OPS-1718" {
		t.Errorf("receipt = %+v", rc)
	}
}

func TestWaitGivesUpWhenNoReceiptArrives(t *testing.T) {
	d := New(t.TempDir())

	rc, ok, err := Wait(context.Background(), d, "a", 20*time.Millisecond, 5*time.Millisecond)

	if err != nil || ok {
		t.Fatalf("Wait = (%+v, %v, %v), want absence without an error", rc, ok, err)
	}
}

func TestSweepRemovesFilesOlderThanTheTTL(t *testing.T) {
	d := New(t.TempDir())
	if _, err := d.Write(request("old")); err != nil {
		t.Fatal(err)
	}
	stale := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(filepath.Join(d.Path(), "old.json"), stale, stale); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Write(request("fresh")); err != nil {
		t.Fatal(err)
	}

	removed, err := d.Sweep(time.Now(), time.Hour)

	if err != nil || removed != 1 {
		t.Fatalf("Sweep = (%d, %v), want 1 removed", removed, err)
	}
	if _, err := os.Stat(filepath.Join(d.Path(), "fresh.json")); err != nil {
		t.Errorf("fresh request must survive: %v", err)
	}
}
