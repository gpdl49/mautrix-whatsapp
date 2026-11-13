package connector

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	_ "github.com/lib/pq"
	"go.mau.fi/util/dbutil"
	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"google.golang.org/protobuf/proto"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
)

func TestDecideDeprecatedHold(t *testing.T) {
	open := deprecatedChat{HoldUntil: time.Now()}
	idle := deprecatedChat{CanReply: true}
	spent := deprecatedChat{}
	cases := []struct {
		name          string
		c             deprecatedChat
		fromMe, count bool
		want          holdDecision
	}{
		{"their message opens a batch", idle, false, true, holdDecision{Hold: true, OpenBatch: true}},
		{"their message opens a batch after reply spent", spent, false, true, holdDecision{Hold: true, OpenBatch: true}},
		{"their reaction between batches goes through", idle, false, false, holdDecision{}},
		{"anything joins an open batch", open, false, false, holdDecision{Hold: true}},
		{"phone message joins an open batch", open, true, true, holdDecision{Hold: true}},
		{"phone message spends the reply", idle, true, true, holdDecision{UseReply: true}},
		{"phone message with no reply left still goes", spent, true, true, holdDecision{}},
		{"phone reaction spends nothing", idle, true, false, holdDecision{}},
	}
	for _, tc := range cases {
		if got := decideDeprecatedHold(tc.c, tc.fromMe, tc.count); got != tc.want {
			t.Errorf("%s: got %+v, want %+v", tc.name, got, tc.want)
		}
	}
}

func TestCountsAsMessage(t *testing.T) {
	cases := map[string]struct {
		msg  *waE2E.Message
		want bool
	}{
		"text":     {&waE2E.Message{Conversation: proto.String("hi")}, true},
		"image":    {&waE2E.Message{ImageMessage: &waE2E.ImageMessage{}}, true},
		"reaction": {&waE2E.Message{ReactionMessage: &waE2E.ReactionMessage{Text: proto.String("👍")}}, false},
		"edit": {&waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{
			Type: waE2E.ProtocolMessage_MESSAGE_EDIT.Enum()}}, false},
		"revoke": {&waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{
			Type: waE2E.ProtocolMessage_REVOKE.Enum(), Key: &waCommon.MessageKey{}}}, false},
		"nil": {nil, false},
	}
	for name, tc := range cases {
		if got := countsAsMessage(tc.msg); got != tc.want {
			t.Errorf("%s: got %v, want %v", name, got, tc.want)
		}
	}
}

// openDeprecateDBs returns the databases to run store tests against: always a
// fresh SQLite file, plus PostgreSQL when HOMESTACKS_TEST_POSTGRES holds a URI
// (production runs on PostgreSQL).
func openDeprecateDBs(t *testing.T) map[string]*dbutil.Database {
	t.Helper()
	out := make(map[string]*dbutil.Database)
	sqlite, err := dbutil.NewWithDialect("file:"+filepath.Join(t.TempDir(), "deprecate.db")+"?_txlock=immediate", "sqlite3-fk-wal")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = sqlite.Close() })
	out["sqlite"] = sqlite
	if uri := os.Getenv("HOMESTACKS_TEST_POSTGRES"); uri != "" {
		pg, err := dbutil.NewWithDialect(uri, "postgres")
		if err != nil {
			t.Fatalf("open postgres: %v", err)
		}
		ctx := context.Background()
		for _, q := range []string{"DROP TABLE IF EXISTS homestacks_deprecated_chat", "DROP TABLE IF EXISTS homestacks_deprecated_event",
			"DROP TABLE IF EXISTS " + deprecateVersionTable} {
			if _, err = pg.Exec(ctx, q); err != nil {
				t.Fatalf("reset postgres: %v", err)
			}
		}
		t.Cleanup(func() { _ = pg.Close() })
		out["postgres"] = pg
	}
	return out
}

func newTestDeprecation(t *testing.T, raw *dbutil.Database) *deprecationState {
	t.Helper()
	db := raw.Child(deprecateVersionTable, deprecateUpgrades, nil)
	if err := db.Upgrade(context.Background()); err != nil {
		t.Fatalf("upgrade: %v", err)
	}
	return &deprecationState{
		store:     &deprecateStore{db: db, bridgeID: "wa"},
		chats:     make(map[networkid.PortalKey]*deprecatedChat),
		releasing: make(map[networkid.PortalKey]bool),
		lastKind:  make(map[networkid.PortalKey]heldKind),
	}
}

func TestDeprecateStoreRoundTrip(t *testing.T) {
	for name, raw := range openDeprecateDBs(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			ds := newTestDeprecation(t, raw)
			dm := networkid.PortalKey{ID: "1@lid", Receiver: "login1"}
			now := time.UnixMilli(time.Now().UnixMilli())

			if _, already, err := ds.deprecate(ctx, dm, now, true); err != nil || already {
				t.Fatalf("deprecate = %v, %v", already, err)
			}
			if c, already, _ := ds.deprecate(ctx, dm, now.Add(time.Hour), false); !already || !c.CanReply || !c.Since.Equal(now) {
				t.Errorf("second deprecate = %+v, %v; want the first state kept", c, already)
			}
			until := now.Add(deprecatedHoldDuration)
			ds.lock.Lock()
			err := ds.update(ctx, dm, func(c *deprecatedChat) { c.HoldUntil, c.CanReply = until, false })
			ds.lock.Unlock()
			if err != nil {
				t.Fatalf("update: %v", err)
			}
			rows := []*deprecatedRow{
				{frozenRow{LoginID: "login1", MessageID: "b", Seq: 2, Event: []byte(`{"n":2}`)}, heldMine},
				{frozenRow{LoginID: "login1", MessageID: "a", Seq: 1, Event: []byte(`{"n":1}`)}, heldTheirs},
				{frozenRow{LoginID: "login1", MessageID: "a", Seq: 3, Event: []byte(`{"n":3}`)}, heldOther}, // redelivery
			}
			for _, r := range rows {
				if err = ds.store.putEvent(ctx, dm, r); err != nil {
					t.Fatalf("putEvent: %v", err)
				}
			}
			got, err := ds.store.listEvents(ctx, dm)
			if err != nil || len(got) != 2 || got[0].MessageID != "a" || got[0].Kind != heldTheirs || got[1].Kind != heldMine {
				t.Fatalf("listEvents = %+v, %v; want a (theirs) then b (mine)", got, err)
			}

			// A restart sees the same state.
			reloaded, err := ds.store.loadChats(ctx)
			if err != nil || len(reloaded) != 1 {
				t.Fatalf("loadChats = %v, %v", reloaded, err)
			}
			if c := reloaded[dm]; !c.Since.Equal(now) || !c.HoldUntil.Equal(until) || c.CanReply {
				t.Errorf("reloaded %+v", c)
			}

			// Draining: rows first, then the batch closes; the last counted
			// message decides the reply.
			for _, r := range got {
				ds.noteReplayed(dm, r.Kind)
				if err = ds.store.deleteEvent(ctx, dm, r); err != nil {
					t.Fatalf("deleteEvent: %v", err)
				}
			}
			if _, done, err := ds.nextBatch(ctx, dm); err != nil || !done {
				t.Fatalf("nextBatch = %v, %v; want done", done, err)
			}
			if c, _ := ds.get(dm); !c.HoldUntil.IsZero() || c.CanReply {
				t.Errorf("after batch ending in a phone message: %+v; want closed, no reply", c)
			}
			reloaded, _ = ds.store.loadChats(ctx)
			if c := reloaded[dm]; !c.HoldUntil.IsZero() || c.CanReply {
				t.Errorf("reloaded after close: %+v", c)
			}
		})
	}
}

func TestDeprecatedSendAllowsOneReplyPerBatch(t *testing.T) {
	raw := openDeprecateDBs(t)["sqlite"]
	ds := newTestDeprecation(t, raw)
	old := deprecated
	deprecated = ds
	t.Cleanup(func() { deprecated = old })

	ctx := context.Background()
	dm := networkid.PortalKey{ID: "1@lid", Receiver: "login1"}
	portal := &bridgev2.Portal{Portal: &database.Portal{PortalKey: dm}}
	wa := &WhatsAppClient{UserLogin: &bridgev2.UserLogin{UserLogin: &database.UserLogin{}}}

	other := &bridgev2.Portal{Portal: &database.Portal{PortalKey: networkid.PortalKey{ID: "2@lid", Receiver: "login1"}}}
	if _, err := wa.checkDeprecatedSend(ctx, other); err != nil {
		t.Fatalf("non-deprecated chat refused: %v", err)
	}
	if _, _, err := ds.deprecate(ctx, dm, time.Now(), false); err != nil {
		t.Fatal(err)
	}
	if _, err := wa.checkDeprecatedSend(ctx, portal); err == nil {
		t.Fatal("send allowed before any batch")
	}

	// A delivered batch from them gives one reply.
	ds.lock.Lock()
	_ = ds.update(ctx, dm, func(c *deprecatedChat) { c.HoldUntil = time.Now() })
	ds.lock.Unlock()
	if _, err := wa.checkDeprecatedSend(ctx, portal); err == nil {
		t.Fatal("send allowed while a batch is held")
	}
	ds.noteReplayed(dm, heldTheirs)
	if _, done, err := ds.nextBatch(ctx, dm); !done || err != nil {
		t.Fatalf("nextBatch = %v, %v", done, err)
	}
	refund, err := wa.checkDeprecatedSend(ctx, portal)
	if err != nil {
		t.Fatalf("reply after batch refused: %v", err)
	}
	if _, err := wa.checkDeprecatedSend(ctx, portal); err == nil {
		t.Fatal("second reply allowed")
	}

	// A failed send gives the reply back; a successful one doesn't.
	sendErr := errors.New("send failed")
	refund(&sendErr)
	refund, err = wa.checkDeprecatedSend(ctx, portal)
	if err != nil {
		t.Fatalf("reply after refund refused: %v", err)
	}
	var ok error
	refund(&ok)
	if _, err := wa.checkDeprecatedSend(ctx, portal); err == nil {
		t.Fatal("reply allowed after a successful send")
	}
	if c, _ := ds.store.loadChats(ctx); c[dm].CanReply {
		t.Error("spent reply not persisted")
	}
}
