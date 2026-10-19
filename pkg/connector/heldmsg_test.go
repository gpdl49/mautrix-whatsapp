// mautrix-whatsapp - A Matrix-WhatsApp puppeting bridge.
// Copyright (C) 2024 Tulir Asokan
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program.  If not, see <https://www.gnu.org/licenses/>.

package connector

import (
	"testing"
	"time"

	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"go.mau.fi/mautrix-whatsapp/pkg/waid"
)

func TestClockMinutes(t *testing.T) {
	good := map[string]int{
		"18:30": 18*60 + 30, "0:00": 0, "8": 8 * 60, "0830": 8*60 + 30, "23:59": 23*60 + 59,
		"8am": 8 * 60, "6:30pm": 18*60 + 30, "12am": 0, "12pm": 12 * 60, "7 PM": 19 * 60,
	}
	for in, want := range good {
		if got, err := clockMinutes(in); err != nil || got != want {
			t.Errorf("clockMinutes(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, in := range []string{"", "24:00", "8:5", "8:60", "13pm", "0am", "noon", "-1", ":30"} {
		if got, err := clockMinutes(in); err == nil {
			t.Errorf("clockMinutes(%q) = %d, want error", in, got)
		}
	}
}

func TestParseSendTime(t *testing.T) {
	loc := time.FixedZone("X", 2*3600)
	now := time.Date(2026, 10, 20, 17, 0, 30, 0, loc)
	cases := map[string]time.Time{
		"18:30":  time.Date(2026, 10, 20, 18, 30, 0, 0, loc),
		"9am":    time.Date(2026, 10, 21, 9, 0, 0, 0, loc),
		"17:00":  time.Date(2026, 10, 21, 17, 0, 0, 0, loc), // already passed this minute
		"+45m":   now.Add(45 * time.Minute).Truncate(time.Second),
		"+2h30m": now.Add(150 * time.Minute).Truncate(time.Second),
		"+24h":   now.Add(24 * time.Hour).Truncate(time.Second),
	}
	for in, want := range cases {
		if got, err := parseSendTime(in, now); err != nil || !got.Equal(want) {
			t.Errorf("parseSendTime(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, in := range []string{"+25h", "+0m", "+-5m", "+soon", "tomorrow"} {
		if _, err := parseSendTime(in, now); err == nil {
			t.Errorf("parseSendTime(%q) succeeded, want error", in)
		}
	}
}

func TestOfflineUntil(t *testing.T) {
	loc := time.FixedZone("X", 0)
	at := func(d, h, m int) time.Time { return time.Date(2026, 10, d, h, m, 0, 0, loc) }
	night, err := parseOfflineHours("22:00-08:00")
	if err != nil {
		t.Fatal(err)
	}
	day := &waid.OfflineHours{Start: 9 * 60, End: 17 * 60}
	cases := []struct {
		oh     *waid.OfflineHours
		now    time.Time
		inside bool
		until  time.Time
	}{
		{night, at(20, 23, 15), true, at(21, 8, 0)},
		{night, at(21, 2, 0), true, at(21, 8, 0)},
		{night, at(21, 7, 59), true, at(21, 8, 0)},
		{night, at(21, 8, 0), false, time.Time{}},
		{night, at(20, 21, 59), false, time.Time{}},
		{night, at(20, 22, 0), true, at(21, 8, 0)},
		{day, at(20, 9, 0), true, at(20, 17, 0)},
		{day, at(20, 16, 59), true, at(20, 17, 0)},
		{day, at(20, 17, 0), false, time.Time{}},
		{day, at(20, 8, 0), false, time.Time{}},
	}
	for _, c := range cases {
		until, inside := offlineUntil(c.oh, c.now)
		if inside != c.inside || !until.Equal(c.until) {
			t.Errorf("offlineUntil(%s, %v) = %v, %v; want %v, %v",
				describeOfflineHours(c.oh), c.now, until, inside, c.until, c.inside)
		}
	}
}

func TestOfflineUntilAcrossDST(t *testing.T) {
	loc, err := time.LoadLocation("Europe/London")
	if err != nil {
		t.Fatal(err)
	}
	// Clocks go back at 02:00 BST on 2026-10-25; the window still ends at 08:00 GMT.
	now := time.Date(2026, 10, 24, 23, 0, 0, 0, loc)
	until, inside := offlineUntil(&waid.OfflineHours{Start: 22 * 60, End: 8 * 60}, now)
	if want := time.Date(2026, 10, 25, 8, 0, 0, 0, loc); !inside || !until.Equal(want) || until.Sub(now) != 10*time.Hour {
		t.Errorf("got %v, %v; want %v (10h away)", until, inside, want)
	}
}

func TestParseOfflineHours(t *testing.T) {
	oh, err := parseOfflineHours("10pm-8am")
	if err != nil || oh.Start != 22*60 || oh.End != 8*60 {
		t.Errorf("parseOfflineHours(10pm-8am) = %+v, %v", oh, err)
	}
	for _, in := range []string{"22:00", "8-8", "22:00-25:00", "-8"} {
		if _, err = parseOfflineHours(in); err == nil {
			t.Errorf("parseOfflineHours(%q) succeeded, want error", in)
		}
	}
}

func TestHeldEventRoundTrip(t *testing.T) {
	// Content as received keeps fields the bridge strips from the parsed copy.
	raw := `{"msgtype":"m.image","body":"cat.jpg","url":"mxc://example.com/abc","info":{"mimetype":"image/jpeg"}}`
	evt := &event.Event{
		Type: event.EventMessage, ID: "$evt", RoomID: "!room:example.com", Sender: "@me:example.com",
		Timestamp: 1700000000000,
	}
	if err := evt.Content.UnmarshalJSON([]byte(raw)); err != nil {
		t.Fatal(err)
	}
	_ = evt.Content.ParseRaw(evt.Type)
	evt.Content.AsMessage().Body = "mutated"
	data, err := encodeHeldEvent(evt)
	if err != nil {
		t.Fatal(err)
	}
	got, err := decodeHeldEvent(data)
	if err != nil {
		t.Fatal(err)
	}
	msg := got.Content.AsMessage()
	if got.Type != event.EventMessage || got.ID != evt.ID || got.RoomID != evt.RoomID || got.Sender != evt.Sender ||
		got.Timestamp != evt.Timestamp || msg.MsgType != event.MsgImage || msg.Body != "cat.jpg" || msg.URL != "mxc://example.com/abc" {
		t.Errorf("round trip lost data: %+v %+v", got, msg)
	}

	// Inline `!wa schedule` text has only parsed content.
	inline := &event.Event{Type: event.EventMessage, ID: "$cmd", Content: event.Content{
		Parsed: &event.MessageEventContent{MsgType: event.MsgText, Body: "hi"},
	}}
	if data, err = encodeHeldEvent(inline); err != nil {
		t.Fatal(err)
	}
	if got, err = decodeHeldEvent(data); err != nil || got.Content.AsMessage().Body != "hi" {
		t.Errorf("inline round trip = %+v, %v", got, err)
	}
}

func TestHeldStateArm(t *testing.T) {
	hs := &heldState{armed: map[armKey]armedSchedule{}, releasing: map[id.EventID]time.Time{}}
	now := time.Now()
	at := now.Add(time.Hour)
	hs.arm("!r", "@u", at, now)
	if _, ok := hs.takeArmed("!r", "@other", now); ok {
		t.Error("armed schedule leaked to another user")
	}
	if got, ok := hs.takeArmed("!r", "@u", now.Add(time.Minute)); !ok || !got.Equal(at) {
		t.Error("armed schedule not returned")
	}
	if _, ok := hs.takeArmed("!r", "@u", now); ok {
		t.Error("armed schedule used twice")
	}
	hs.arm("!r", "@u", at, now)
	if _, ok := hs.takeArmed("!r", "@u", now.Add(heldArmTTL+time.Second)); ok {
		t.Error("expired armed schedule used")
	}

	hs.markReleasing("$a", now)
	if !hs.takeReleasing("$a") || hs.takeReleasing("$a") {
		t.Error("releasing mark should pass exactly once")
	}
}
