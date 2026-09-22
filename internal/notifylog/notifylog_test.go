package notifylog

import (
	"testing"
	"time"
)

const validBlock = `[09/10/26, 17:10:40:488] info: Store: NEW_NOTIFICATION {
  "title": "[REDACTED]",
  "teamId": "T100TEAM",
  "userId": "U100SELF",
  "msg": "1789049440.051109",
  "channel": "C100HOME",
  "thread_ts": "1789047527.174689",
  "silent": false,
  "id": "T100TEAM_1789049440.051109",
  "mac": {
    "closeButtonOverride": false
  }
}`

func feedAll(t *testing.T, text string) []Notification {
	t.Helper()
	s := NewScanner()
	var got []Notification
	for _, line := range splitLines(text) {
		if n, ok := s.Feed(line); ok {
			got = append(got, n)
		}
	}
	return got
}

func splitLines(text string) []string {
	var lines []string
	start := 0
	for i := 0; i < len(text); i++ {
		if text[i] == '\n' {
			lines = append(lines, text[start:i])
			start = i + 1
		}
	}
	return append(lines, text[start:])
}

func TestParsesCompleteNotification(t *testing.T) {
	got := feedAll(t, validBlock)
	if len(got) != 1 {
		t.Fatalf("want 1 notification, got %d", len(got))
	}
	n := got[0]
	if n.Channel != "C100HOME" {
		t.Errorf("channel = %q", n.Channel)
	}
	if n.MsgTS != "1789049440.051109" {
		t.Errorf("msg = %q", n.MsgTS)
	}
	if n.ThreadTS != "1789047527.174689" {
		t.Errorf("thread_ts = %q", n.ThreadTS)
	}
	if n.ID != "T100TEAM_1789049440.051109" {
		t.Errorf("id = %q", n.ID)
	}
}

func TestIgnoresUnrelatedLines(t *testing.T) {
	text := "[09/10/26, 17:08:18:404] info: [NOTIFICATIONS] SuppressNotificationReason: NONE\n" +
		"[09/10/26, 17:08:18:405] info: [COUNTS] Updated unread_cnt\n"
	if got := feedAll(t, text); len(got) != 0 {
		t.Fatalf("want no notifications, got %d", len(got))
	}
}

func TestDropsBlockInterruptedByNextLogEntry(t *testing.T) {
	text := "[09/10/26, 17:10:40:488] info: Store: NEW_NOTIFICATION {\n" +
		"  \"channel\": \"C100HOME\",\n" +
		"[09/10/26, 17:10:41:000] info: [COUNTS] Updated unread_cnt\n"
	if got := feedAll(t, text); len(got) != 0 {
		t.Fatalf("want no notifications from truncated block, got %d", len(got))
	}
}

func TestRecoversWhenNextBlockStartsImmediately(t *testing.T) {
	text := "[09/10/26, 17:10:40:488] info: Store: NEW_NOTIFICATION {\n" +
		"  \"channel\": \"C100HOME\",\n" +
		validBlock
	got := feedAll(t, text)
	if len(got) != 1 {
		t.Fatalf("want 1 notification after recovery, got %d", len(got))
	}
	if got[0].MsgTS != "1789049440.051109" {
		t.Errorf("msg = %q", got[0].MsgTS)
	}
}

func TestDropsBlockWithoutRequiredFields(t *testing.T) {
	text := "[09/10/26, 17:10:40:488] info: Store: NEW_NOTIFICATION {\n" +
		"  \"title\": \"[REDACTED]\"\n" +
		"}\n"
	if got := feedAll(t, text); len(got) != 0 {
		t.Fatalf("want no notifications, got %d", len(got))
	}
}

func TestDropsMalformedJSON(t *testing.T) {
	text := "[09/10/26, 17:10:40:488] info: Store: NEW_NOTIFICATION {\n" +
		"  \"channel\": \"C100HOME\",,,\n" +
		"}\n"
	if got := feedAll(t, text); len(got) != 0 {
		t.Fatalf("want no notifications, got %d", len(got))
	}
}

func TestRootTSFallsBackToMessageTS(t *testing.T) {
	text := "[09/10/26, 17:10:40:488] info: Store: NEW_NOTIFICATION {\n" +
		"  \"msg\": \"1789049440.051109\",\n" +
		"  \"channel\": \"C100HOME\",\n" +
		"  \"thread_ts\": null,\n" +
		"  \"id\": \"T100_1789049440.051109\"\n" +
		"}\n"
	got := feedAll(t, text)
	if len(got) != 1 {
		t.Fatalf("want 1 notification, got %d", len(got))
	}
	if got[0].RootTS() != "1789049440.051109" {
		t.Errorf("RootTS = %q", got[0].RootTS())
	}
}

const lastReadLine = "[09/11/26, 20:17:10:690] info: [SET-LAST-READ] (T0000000001) markLastRead D0000000001:1789146698.916139, immediate: false "

func TestLastReadYieldsNotificationWithSameIDShapeAsBanner(t *testing.T) {
	now := func() time.Time { return time.Unix(1789146698+30, 0) }
	s := NewLastReadScanner(2*time.Minute, now)

	got, ok := s.Feed(lastReadLine)

	if !ok {
		t.Fatal("fresh markLastRead must become a notification")
	}
	if got.ID != "T0000000001_1789146698.916139" || got.Channel != "D0000000001" || got.MsgTS != "1789146698.916139" {
		t.Errorf("notification = %+v", got)
	}
	if got.Source != SourceLastRead || got.TeamID != "T0000000001" || got.ThreadTS != "" {
		t.Errorf("notification = %+v", got)
	}
}

func TestLastReadIgnoresOldMessagesScrolledInto(t *testing.T) {
	now := func() time.Time { return time.Unix(1789146698+3600, 0) }

	if _, ok := NewLastReadScanner(2*time.Minute, now).Feed(lastReadLine); ok {
		t.Error("marking an hour-old message as read is scrolling, not a new message")
	}
}

func TestLastReadIgnoresOtherLinesAndDisabledScanner(t *testing.T) {
	now := func() time.Time { return time.Unix(1789146698+5, 0) }
	s := NewLastReadScanner(2*time.Minute, now)

	for _, line := range []string{
		"[09/11/26, 20:17:10:690] info: [SET-LAST-READ] (T0000000001) cancelling delayed mark for C0000000001 ",
		"[09/11/26, 20:17:10:690] info: [COUNTS] (T0000000001) Threads has_unreads:false mention_count:0 ",
		"",
	} {
		if _, ok := s.Feed(line); ok {
			t.Errorf("line %q must not yield a notification", line)
		}
	}
	if _, ok := NewLastReadScanner(0, now).Feed(lastReadLine); ok {
		t.Error("zero max age disables the scanner")
	}
}

func TestLastReadAndBannerDeduplicateThroughTheSameID(t *testing.T) {
	banner := NewScanner()
	var fromBanner Notification
	for _, line := range []string{
		"[09/11/26, 20:17:09:000] info: Store: NEW_NOTIFICATION {",
		`  "id": "T0000000001_1789146698.916139",`,
		`  "teamId": "T0000000001",`,
		`  "channel": "D0000000001",`,
		`  "msg": "1789146698.916139"`,
		"}",
	} {
		if n, ok := banner.Feed(line); ok {
			fromBanner = n
		}
	}
	now := func() time.Time { return time.Unix(1789146698+5, 0) }
	fromRead, _ := NewLastReadScanner(2*time.Minute, now).Feed(lastReadLine)

	if fromBanner.ID == "" || fromBanner.ID != fromRead.ID {
		t.Errorf("ids differ: banner %q read %q", fromBanner.ID, fromRead.ID)
	}
}
