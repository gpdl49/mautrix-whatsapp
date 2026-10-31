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
	"context"
	"path/filepath"
	"testing"
	"time"

	"go.mau.fi/util/dbutil"
	_ "go.mau.fi/util/dbutil/litestream"
	"go.mau.fi/util/ptr"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"maunium.net/go/mautrix/bridgev2/networkid"
)

func TestFrozenEventRoundTrip(t *testing.T) {
	ts := time.Unix(1761390000, 0)
	orig := &events.Message{
		Info: types.MessageInfo{
			MessageSource: types.MessageSource{
				Chat:      types.NewJID("15550000001", types.DefaultUserServer),
				Sender:    types.NewJID("15550000001", types.DefaultUserServer),
				SenderAlt: types.NewJID("123456789", types.HiddenUserServer),
			},
			ID:        "3EB0ABC",
			PushName:  "Alice",
			Timestamp: ts,
		},
		RawMessage: &waE2E.Message{EphemeralMessage: &waE2E.FutureProofMessage{Message: &waE2E.Message{
			Conversation: ptr.Ptr("hello"),
		}}},
		UnavailableRequestID: "req1",
		RetryCount:           2,
	}
	orig.UnwrapRaw()
	data, err := encodeFrozenEvent(orig)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	got, err := decodeFrozenEvent(data)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Info.Chat != orig.Info.Chat || got.Info.SenderAlt != orig.Info.SenderAlt || got.Info.ID != orig.Info.ID ||
		!got.Info.Timestamp.Equal(ts) || got.Info.PushName != "Alice" {
		t.Errorf("info changed: %+v", got.Info)
	}
	if got.Message.GetConversation() != "hello" || !got.IsEphemeral {
		t.Errorf("message not unwrapped: %v, ephemeral %v", got.Message, got.IsEphemeral)
	}
	if got.UnavailableRequestID != "req1" || got.RetryCount != 2 {
		t.Errorf("metadata lost: %q %d", got.UnavailableRequestID, got.RetryCount)
	}
}

func TestFreezeNextSeqIsStrictlyIncreasing(t *testing.T) {
	fs := &freezeState{}
	now := time.Unix(1761390000, 0)
	a, b := fs.nextSeq(now), fs.nextSeq(now)
	if b <= a {
		t.Errorf("same instant: %d then %d", a, b)
	}
	if c := fs.nextSeq(now.Add(-time.Second)); c <= b {
		t.Errorf("clock went back: %d after %d", c, b)
	}
}

func TestFreezeStoreRoundTrip(t *testing.T) {
	ctx := context.Background()
	raw, err := dbutil.NewWithDialect("file:"+filepath.Join(t.TempDir(), "freeze.db")+"?_txlock=immediate", "sqlite3-fk-wal")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer raw.Close()
	db := raw.Child(freezeVersionTable, freezeUpgrades, nil)
	if err = db.Upgrade(ctx); err != nil {
		t.Fatalf("upgrade: %v", err)
	}
	s := &freezeStore{db: db, bridgeID: "wa"}
	other := &freezeStore{db: db, bridgeID: "other-bridge"}
	dm := networkid.PortalKey{ID: "lid-1", Receiver: "login1"}
	group := networkid.PortalKey{ID: "123@g.us"}
	now := time.UnixMilli(time.Now().UnixMilli())

	for _, key := range []networkid.PortalKey{dm, group} {
		if err = s.putChat(ctx, key, now); err != nil {
			t.Fatalf("putChat: %v", err)
		}
	}
	if err = s.putChat(ctx, dm, now.Add(time.Hour)); err != nil {
		t.Fatalf("putChat again: %v", err)
	}
	chats, err := s.loadChats(ctx)
	if err != nil || len(chats) != 2 || !chats[dm].Equal(now) {
		t.Fatalf("loadChats = %v, %v; want both, dm at its first freeze", chats, err)
	}
	if theirs, _ := other.loadChats(ctx); len(theirs) != 0 {
		t.Errorf("other bridge sees %v", theirs)
	}

	rows := []*frozenRow{
		{LoginID: "login1", MessageID: "b", Seq: 2, Event: []byte(`{"n":2}`)},
		{LoginID: "login1", MessageID: "a", Seq: 1, Event: []byte(`{"n":1}`)},
		{LoginID: "login1", MessageID: "a", Seq: 3, Event: []byte(`{"n":3}`)}, // redelivery
	}
	for _, r := range rows {
		if err = s.putEvent(ctx, dm, r); err != nil {
			t.Fatalf("putEvent: %v", err)
		}
	}
	got, err := s.listEvents(ctx, dm)
	if err != nil || len(got) != 2 || got[0].MessageID != "a" || string(got[0].Event) != `{"n":1}` || got[1].MessageID != "b" {
		t.Fatalf("listEvents = %+v, %v; want a then b, redelivery ignored", got, err)
	}
	if n, _ := s.countEvents(ctx, group); n != 0 {
		t.Errorf("group has %d events", n)
	}
	if err = s.deleteEvent(ctx, dm, got[0]); err != nil {
		t.Fatalf("deleteEvent: %v", err)
	}
	if n, _ := s.countEvents(ctx, dm); n != 1 {
		t.Errorf("after delete: %d events", n)
	}
	if err = s.deleteChat(ctx, dm); err != nil {
		t.Fatalf("deleteChat: %v", err)
	}
	if chats, _ = s.loadChats(ctx); len(chats) != 1 {
		t.Errorf("after deleteChat: %v", chats)
	}
}

func TestFreezeNextBatchUnfreezesWhenEmpty(t *testing.T) {
	ctx := context.Background()
	raw, err := dbutil.NewWithDialect("file:"+filepath.Join(t.TempDir(), "freeze.db")+"?_txlock=immediate", "sqlite3-fk-wal")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer raw.Close()
	db := raw.Child(freezeVersionTable, freezeUpgrades, nil)
	if err = db.Upgrade(ctx); err != nil {
		t.Fatalf("upgrade: %v", err)
	}
	fs := &freezeState{
		store:   &freezeStore{db: db, bridgeID: "wa"},
		chats:   make(map[networkid.PortalKey]time.Time),
		thawing: make(map[networkid.PortalKey]bool),
	}
	key := networkid.PortalKey{ID: "lid-1", Receiver: "login1"}
	if _, already, err := fs.freeze(ctx, key, time.Now()); err != nil || already {
		t.Fatalf("freeze = %v, %v", already, err)
	}
	row := &frozenRow{LoginID: "login1", MessageID: "a", Seq: 1, Event: []byte(`{}`)}
	if err = fs.store.putEvent(ctx, key, row); err != nil {
		t.Fatalf("putEvent: %v", err)
	}
	rows, done, err := fs.nextBatch(ctx, key)
	if err != nil || done || len(rows) != 1 {
		t.Fatalf("first batch = %d rows, done %v, %v; want the held row", len(rows), done, err)
	}
	if !fs.startThaw(key) || fs.startThaw(key) {
		t.Error("second concurrent thaw was allowed")
	}
	fs.endThaw(key)
	if err = fs.store.deleteEvent(ctx, key, row); err != nil {
		t.Fatalf("deleteEvent: %v", err)
	}
	if _, done, err = fs.nextBatch(ctx, key); err != nil || !done {
		t.Fatalf("empty batch: done %v, %v", done, err)
	}
	if _, ok := fs.since(key); ok {
		t.Error("chat still frozen in memory")
	}
	if chats, _ := fs.store.loadChats(ctx); len(chats) != 0 {
		t.Errorf("chat still frozen in DB: %v", chats)
	}
}
