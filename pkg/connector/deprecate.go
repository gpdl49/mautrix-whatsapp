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
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"
	"go.mau.fi/util/dbutil"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types/events"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/networkid"

	"go.mau.fi/mautrix-whatsapp/pkg/waid"
)

// This file implements the homestacks deprecated chats (`!wa deprecate`). A
// deprecated DM works in rounds:
//
//  1. Their first message after a round ends opens a batch: it and everything
//     else in the chat is held (stored, not bridged) for deprecatedHoldDuration.
//  2. When the batch is due it is replayed through the normal message handler,
//     in arrival order with original timestamps, like an unfreeze.
//  3. Once it is delivered, the user may send exactly one message from Matrix.
//     Any further message is refused until the next batch has been delivered.
//
// It is keyed by portal key: a DM's key is the contact's LID plus the login,
// which is the same for every room the chat ever gets. Deleting the chat
// deletes the room but not the row here, so a new chat with them, whoever
// starts it, is deprecated from its first message.
//
// There is deliberately no command, config option or metadata flag that
// undoes it. Undeprecating takes a code change: see HOMESTACKS.md.

// deprecatedHoldDuration is how long a batch of their messages is held,
// counted from the first message of the batch.
const deprecatedHoldDuration = 5 * time.Minute

// deprecatedReleaseInterval is how often due batches are looked for.
const deprecatedReleaseInterval = 15 * time.Second

// deprecatedGiveUpAfter is how long past its due time a held message that
// keeps failing to replay is retried before it is dropped. Without it one bad
// message would hold the chat forever, and there is no command to clear it.
const deprecatedGiveUpAfter = time.Hour

const deprecateVersionTable = "homestacks_deprecate_version"

var deprecateUpgrades = dbutil.BuildUpgradeTable().
	WithRaw(0, 1, 0, "Add homestacks deprecated chats", dbutil.TxnModeOn, func(ctx context.Context, db *dbutil.Database) error {
		if _, err := db.Exec(ctx, `CREATE TABLE homestacks_deprecated_chat (
			bridge_id       TEXT    NOT NULL,
			portal_id       TEXT    NOT NULL,
			portal_receiver TEXT    NOT NULL,
			deprecated_at   BIGINT  NOT NULL,
			hold_until      BIGINT  NOT NULL,
			can_reply       BOOLEAN NOT NULL,
			PRIMARY KEY (bridge_id, portal_id, portal_receiver)
		)`); err != nil {
			return err
		}
		_, err := db.Exec(ctx, `CREATE TABLE homestacks_deprecated_event (
			bridge_id       TEXT     NOT NULL,
			portal_id       TEXT     NOT NULL,
			portal_receiver TEXT     NOT NULL,
			login_id        TEXT     NOT NULL,
			message_id      TEXT     NOT NULL,
			seq             BIGINT   NOT NULL,
			kind            SMALLINT NOT NULL,
			event_json      TEXT     NOT NULL,
			PRIMARY KEY (bridge_id, portal_id, portal_receiver, login_id, message_id)
		)`)
		return err
	}).
	Finish()

type deprecateReplayKey struct{}

// heldKind says whether a held message counts towards the reply rule.
type heldKind int16

const (
	heldOther  heldKind = 0 // reactions, edits, deletes and the like
	heldTheirs heldKind = 1
	heldMine   heldKind = 2 // sent from the phone
)

// deprecatedChat is one deprecated chat's state. HoldUntil is zero between
// batches; CanReply is whether the user's one reply is still unused.
type deprecatedChat struct {
	Since     time.Time
	HoldUntil time.Time
	CanReply  bool
}

type deprecatedRow struct {
	frozenRow
	Kind heldKind
}

type deprecateStore struct {
	db       *dbutil.Database
	bridgeID networkid.BridgeID
}

func timeOrZero(ms int64) time.Time {
	if ms == 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms)
}

func msOrZero(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixMilli()
}

func (s *deprecateStore) loadChats(ctx context.Context) (map[networkid.PortalKey]*deprecatedChat, error) {
	rows, err := s.db.Query(ctx, `SELECT portal_id, portal_receiver, deprecated_at, hold_until, can_reply
		FROM homestacks_deprecated_chat WHERE bridge_id=$1`, s.bridgeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[networkid.PortalKey]*deprecatedChat)
	for rows.Next() {
		var key networkid.PortalKey
		var since, until int64
		var c deprecatedChat
		if err = rows.Scan(&key.ID, &key.Receiver, &since, &until, &c.CanReply); err != nil {
			return nil, err
		}
		c.Since, c.HoldUntil = time.UnixMilli(since), timeOrZero(until)
		out[key] = &c
	}
	return out, rows.Err()
}

// putChat writes a chat's state. It only ever inserts or updates: nothing in
// the fork deletes a deprecated chat.
func (s *deprecateStore) putChat(ctx context.Context, key networkid.PortalKey, c *deprecatedChat) error {
	_, err := s.db.Exec(ctx, `INSERT INTO homestacks_deprecated_chat
		(bridge_id, portal_id, portal_receiver, deprecated_at, hold_until, can_reply)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (bridge_id, portal_id, portal_receiver) DO UPDATE
		SET hold_until=excluded.hold_until, can_reply=excluded.can_reply`,
		s.bridgeID, key.ID, key.Receiver, c.Since.UnixMilli(), msOrZero(c.HoldUntil), c.CanReply)
	return err
}

func (s *deprecateStore) putEvent(ctx context.Context, key networkid.PortalKey, r *deprecatedRow) error {
	_, err := s.db.Exec(ctx, `INSERT INTO homestacks_deprecated_event
		(bridge_id, portal_id, portal_receiver, login_id, message_id, seq, kind, event_json)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8) ON CONFLICT DO NOTHING`,
		s.bridgeID, key.ID, key.Receiver, r.LoginID, r.MessageID, r.Seq, int16(r.Kind), string(r.Event))
	return err
}

func (s *deprecateStore) deleteEvent(ctx context.Context, key networkid.PortalKey, r *deprecatedRow) error {
	_, err := s.db.Exec(ctx, `DELETE FROM homestacks_deprecated_event
		WHERE bridge_id=$1 AND portal_id=$2 AND portal_receiver=$3 AND login_id=$4 AND message_id=$5`,
		s.bridgeID, key.ID, key.Receiver, r.LoginID, r.MessageID)
	return err
}

func (s *deprecateStore) listEvents(ctx context.Context, key networkid.PortalKey) ([]*deprecatedRow, error) {
	rows, err := s.db.Query(ctx, `SELECT login_id, message_id, seq, kind, event_json FROM homestacks_deprecated_event
		WHERE bridge_id=$1 AND portal_id=$2 AND portal_receiver=$3 ORDER BY seq`, s.bridgeID, key.ID, key.Receiver)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*deprecatedRow
	for rows.Next() {
		var r deprecatedRow
		var evt string
		var kind int16
		if err = rows.Scan(&r.LoginID, &r.MessageID, &r.Seq, &kind, &evt); err != nil {
			return nil, err
		}
		r.Kind, r.Event = heldKind(kind), []byte(evt)
		out = append(out, &r)
	}
	return out, rows.Err()
}

func (s *deprecateStore) countEvents(ctx context.Context, key networkid.PortalKey) (n int, err error) {
	err = s.db.QueryRow(ctx, `SELECT COUNT(*) FROM homestacks_deprecated_event
		WHERE bridge_id=$1 AND portal_id=$2 AND portal_receiver=$3`, s.bridgeID, key.ID, key.Receiver).Scan(&n)
	return
}

// deprecationState is the in-memory copy of the deprecated chats. lock is held
// for single operations, never across a replay.
type deprecationState struct {
	lock      sync.Mutex
	store     *deprecateStore
	chats     map[networkid.PortalKey]*deprecatedChat
	releasing map[networkid.PortalKey]bool
	// lastKind is the kind of the last counted message replayed in the batch
	// being released, which decides CanReply once it is out.
	lastKind map[networkid.PortalKey]heldKind
	lastSeq  int64
}

var deprecated = &deprecationState{
	chats:     make(map[networkid.PortalKey]*deprecatedChat),
	releasing: make(map[networkid.PortalKey]bool),
	lastKind:  make(map[networkid.PortalKey]heldKind),
}

func (ds *deprecationState) get(key networkid.PortalKey) (deprecatedChat, bool) {
	ds.lock.Lock()
	defer ds.lock.Unlock()
	c, ok := ds.chats[key]
	if !ok {
		return deprecatedChat{}, false
	}
	return *c, true
}

func (ds *deprecationState) list() map[networkid.PortalKey]deprecatedChat {
	ds.lock.Lock()
	defer ds.lock.Unlock()
	out := make(map[networkid.PortalKey]deprecatedChat, len(ds.chats))
	for k, v := range ds.chats {
		out[k] = *v
	}
	return out
}

// nextSeq orders held messages by arrival. Callers hold lock.
func (ds *deprecationState) nextSeq(now time.Time) int64 {
	seq := now.UnixNano()
	if seq <= ds.lastSeq {
		seq = ds.lastSeq + 1
	}
	ds.lastSeq = seq
	return seq
}

// update applies fn to a copy of the chat's state and stores it; the memory
// copy only changes once the database has.
func (ds *deprecationState) update(ctx context.Context, key networkid.PortalKey, fn func(c *deprecatedChat)) error {
	c, ok := ds.chats[key]
	if !ok {
		return nil
	}
	next := *c
	fn(&next)
	if err := ds.store.putChat(ctx, key, &next); err != nil {
		return err
	}
	*c = next
	return nil
}

// deprecate marks a chat deprecated. canReply is whether the user starts
// with a reply in hand (their message was the last one in the chat).
func (ds *deprecationState) deprecate(ctx context.Context, key networkid.PortalKey, now time.Time, canReply bool) (c deprecatedChat, already bool, err error) {
	ds.lock.Lock()
	defer ds.lock.Unlock()
	if existing, ok := ds.chats[key]; ok {
		return *existing, true, nil
	}
	c = deprecatedChat{Since: now, CanReply: canReply}
	if err = ds.store.putChat(ctx, key, &c); err != nil {
		return deprecatedChat{}, false, err
	}
	ds.chats[key] = &c
	return c, false, nil
}

// countsAsMessage is whether a WhatsApp message is a message in its own right
// (opens a batch, uses up a reply) rather than a reaction, edit, delete or
// other change to an existing one.
func countsAsMessage(msg *waE2E.Message) bool {
	switch t := getMessageType(msg); {
	case t == "ignore", t == "reaction", t == "reaction remove", t == "encrypted reaction",
		t == "poll update", t == "edit", t == "revoke", t == "disappearing timer change",
		strings.HasPrefix(t, "unknown_protocol"):
		return false
	}
	return true
}

// holdDecision is what checkDeprecated does with a message, decided from the
// chat's state alone so it can be tested without a database.
type holdDecision struct {
	Hold      bool
	OpenBatch bool
	// UseReply means a message from the phone is going through now and
	// spends the user's reply.
	UseReply bool
}

func decideDeprecatedHold(c deprecatedChat, fromMe, counts bool) holdDecision {
	switch {
	case !c.HoldUntil.IsZero():
		return holdDecision{Hold: true}
	case fromMe:
		return holdDecision{UseReply: counts && c.CanReply}
	case counts:
		return holdDecision{Hold: true, OpenBatch: true}
	default:
		return holdDecision{}
	}
}

// checkDeprecated holds a WhatsApp message in a deprecated chat. Called from
// handleWAMessage right after checkFrozen, so a frozen chat's messages are
// held by the freeze first and come through here when it is unfrozen.
// Returns true if the message was held and must not be bridged now.
func (wa *WhatsAppClient) checkDeprecated(ctx context.Context, evt *events.Message) bool {
	if ctx.Value(deprecateReplayKey{}) != nil {
		return false
	}
	key := (&MessageInfoWrapper{Info: evt.Info, wa: wa}).GetPortalKey()
	deprecated.lock.Lock()
	defer deprecated.lock.Unlock()
	c, ok := deprecated.chats[key]
	if !ok || deprecated.store == nil {
		return false
	}
	log := zerolog.Ctx(ctx).With().Str("message_id", evt.Info.ID).Str("portal_id", string(key.ID)).Logger()
	counts := countsAsMessage(evt.Message)
	d := decideDeprecatedHold(*c, evt.Info.IsFromMe, counts)
	if d.UseReply {
		if err := deprecated.update(ctx, key, func(c *deprecatedChat) { c.CanReply = false }); err != nil {
			log.Err(err).Msg("Failed to record reply sent from phone in deprecated chat")
		}
	}
	if !d.Hold {
		return false
	}
	now := time.Now()
	if d.OpenBatch {
		if err := deprecated.update(ctx, key, func(c *deprecatedChat) { c.HoldUntil = now.Add(deprecatedHoldDuration) }); err != nil {
			// Fail open: bridging a message early beats losing it.
			log.Err(err).Msg("Failed to open batch in deprecated chat, bridging message now")
			return false
		}
	}
	kind := heldOther
	if counts && evt.Info.IsFromMe {
		kind = heldMine
		if evt.Info.Timestamp.Before(c.HoldUntil.Add(-deprecatedHoldDuration)) {
			// Sent before their batch started (a late echo): it answered
			// an earlier batch and doesn't use up this one's reply.
			kind = heldOther
		}
	} else if counts {
		kind = heldTheirs
	}
	data, err := encodeFrozenEvent(evt)
	if err == nil {
		err = deprecated.store.putEvent(ctx, key, &deprecatedRow{
			frozenRow: frozenRow{LoginID: wa.UserLogin.ID, MessageID: evt.Info.ID, Seq: deprecated.nextSeq(now), Event: data},
			Kind:      kind,
		})
	}
	if err != nil {
		log.Err(err).Msg("Failed to hold message in deprecated chat, bridging it now")
		return false
	}
	log.Debug().Time("hold_until", c.HoldUntil).Msg("Held message in deprecated chat")
	return true
}

func (wa *WhatsAppClient) deprecatedRefusal(c deprecatedChat) error {
	var msg string
	if !c.HoldUntil.IsZero() {
		msg = fmt.Sprintf("this chat is deprecated. Their new messages are held until %s; you can reply once after they arrive.",
			c.HoldUntil.In(userLocation(wa.UserLogin)).Format("15:04 MST"))
	} else {
		msg = "this chat is deprecated and you have already sent your one reply. You can send another after their next messages arrive."
	}
	return bridgev2.WrapErrorInStatus(errors.New(msg)).WithErrorAsMessage().WithIsCertain(true).WithSendNotice(true)
}

// checkDeprecatedSend lets one Matrix message through per delivered batch and
// refuses the rest. Called from HandleMatrixMessage and HandleMatrixPollStart
// after the other homestacks guards, so a scheduled message is checked when it
// is sent, not when it is scheduled. The reply is spent before sending (two
// quick messages can't both get through); the returned refund gives it back
// if the send then fails, and must be deferred with the handler's error.
func (wa *WhatsAppClient) checkDeprecatedSend(ctx context.Context, portal *bridgev2.Portal) (refund func(*error), err error) {
	noop := func(*error) {}
	key := portal.PortalKey
	deprecated.lock.Lock()
	defer deprecated.lock.Unlock()
	c, ok := deprecated.chats[key]
	if !ok {
		return noop, nil
	}
	if !c.HoldUntil.IsZero() || !c.CanReply {
		return noop, wa.deprecatedRefusal(*c)
	}
	if err = deprecated.update(ctx, key, func(c *deprecatedChat) { c.CanReply = false }); err != nil {
		// Fail closed: there is no other way to turn this off.
		return noop, fmt.Errorf("failed to record reply in deprecated chat: %w", err)
	}
	return func(sendErr *error) {
		if *sendErr == nil {
			return
		}
		deprecated.lock.Lock()
		defer deprecated.lock.Unlock()
		if c, ok := deprecated.chats[key]; ok && c.HoldUntil.IsZero() {
			if err := deprecated.update(ctx, key, func(c *deprecatedChat) { c.CanReply = true }); err != nil {
				zerolog.Ctx(ctx).Err(err).Msg("Failed to give back reply after failed send in deprecated chat")
			}
		}
	}, nil
}

// lastMessageIsTheirs says whether the newest real message in a chat is
// from the other side, which decides whether a newly deprecated chat starts
// with a reply in hand.
func (wa *WhatsAppClient) lastMessageIsTheirs(ctx context.Context, key networkid.PortalKey) (bool, error) {
	msgs, err := wa.Main.Bridge.DB.Message.GetLastNInPortal(ctx, key, 20)
	if err != nil {
		return false, err
	}
	own := []networkid.UserID{waid.MakeUserID(wa.JID.ToNonAD())}
	if lid := wa.GetLID(); !lid.IsEmpty() {
		own = append(own, waid.MakeUserID(lid.ToNonAD()))
	}
	for _, msg := range msgs {
		if msg.SenderID == "" || strings.HasPrefix(string(msg.MXID), "~fake:") {
			continue
		}
		for _, id := range own {
			if msg.SenderID == id {
				return false, nil
			}
		}
		return true, nil
	}
	return false, nil
}

// nextBatch returns the rows of a due batch, or, if there are none left,
// closes the batch and reports done. Doing both under the lock means a message
// can't slip in between the last replay and the close.
func (ds *deprecationState) nextBatch(ctx context.Context, key networkid.PortalKey) (rows []*deprecatedRow, done bool, err error) {
	ds.lock.Lock()
	defer ds.lock.Unlock()
	c, ok := ds.chats[key]
	if !ok || c.HoldUntil.IsZero() {
		return nil, true, nil
	}
	if rows, err = ds.store.listEvents(ctx, key); err != nil || len(rows) > 0 {
		return rows, false, err
	}
	last := ds.lastKind[key]
	err = ds.update(ctx, key, func(c *deprecatedChat) {
		c.HoldUntil = time.Time{}
		// A batch is opened by one of their messages, so unless the phone
		// answered after it (or the bridge restarted mid-release and lost
		// track) the user has a reply.
		c.CanReply = last != heldMine
	})
	if err != nil {
		return nil, false, err
	}
	delete(ds.lastKind, key)
	return nil, true, nil
}

func (ds *deprecationState) noteReplayed(key networkid.PortalKey, kind heldKind) {
	if kind == heldOther {
		return
	}
	ds.lock.Lock()
	defer ds.lock.Unlock()
	ds.lastKind[key] = kind
}

func (ds *deprecationState) startRelease(key networkid.PortalKey) bool {
	ds.lock.Lock()
	defer ds.lock.Unlock()
	if ds.releasing[key] {
		return false
	}
	ds.releasing[key] = true
	return true
}

func (ds *deprecationState) endRelease(key networkid.PortalKey) {
	ds.lock.Lock()
	defer ds.lock.Unlock()
	delete(ds.releasing, key)
}

// releaseDeprecated replays a due batch in order and closes it. Messages that
// arrive meanwhile join the batch. If one can't be replayed the rest wait for
// the next pass, unless it is deprecatedGiveUpAfter overdue, when it is
// dropped so the chat isn't stuck.
func (wc *WhatsAppConnector) releaseDeprecated(ctx context.Context, key networkid.PortalKey, holdUntil, now time.Time) error {
	if !deprecated.startRelease(key) {
		return nil
	}
	defer deprecated.endRelease(key)
	log := zerolog.Ctx(ctx).With().Str("portal_id", string(key.ID)).Logger()
	giveUp := now.Sub(holdUntil) > deprecatedGiveUpAfter
	replayed := 0
	for {
		rows, done, err := deprecated.nextBatch(ctx, key)
		if err != nil {
			return err
		} else if done {
			log.Info().Int("replayed", replayed).Msg("Delivered held batch of deprecated chat")
			return nil
		}
		for _, r := range rows {
			// An offline login is waited for however long it takes; only a
			// deleted one (nil, nil) drops its messages.
			wa, err := wc.frozenClient(ctx, r.LoginID)
			if err != nil {
				return err
			}
			evt, decodeErr := decodeFrozenEvent(r.Event)
			if wa == nil || decodeErr != nil {
				log.Warn().AnErr("decode_error", decodeErr).Str("message_id", r.MessageID).
					Msg("Dropping held message of deprecated chat")
				if err = deprecated.store.deleteEvent(ctx, key, r); err != nil {
					return err
				}
				continue
			}
			replayCtx := wa.UserLogin.Log.WithContext(context.WithValue(wc.Bridge.BackgroundCtx, deprecateReplayKey{}, true))
			if !wa.handleWAMessage(replayCtx, evt) {
				if !giveUp {
					return fmt.Errorf("message %s could not be bridged", r.MessageID)
				}
				log.Error().Str("message_id", r.MessageID).Msg("Dropping held message of deprecated chat after repeated failures")
			} else {
				deprecated.noteReplayed(key, r.Kind)
				replayed++
			}
			if err = deprecated.store.deleteEvent(ctx, key, r); err != nil {
				return err
			}
		}
	}
}

func (wc *WhatsAppConnector) releaseDueDeprecated(ctx context.Context, now time.Time) {
	for key, c := range deprecated.list() {
		if c.HoldUntil.IsZero() || c.HoldUntil.After(now) {
			continue
		}
		if err := wc.releaseDeprecated(ctx, key, c.HoldUntil, now); err != nil {
			zerolog.Ctx(ctx).Warn().Err(err).Str("portal_id", string(key.ID)).
				Msg("Couldn't deliver held batch of deprecated chat yet, will retry")
		}
	}
}

// startDeprecated upgrades the deprecated chat tables, loads them and starts
// the release loop. A failure here stops the bridge rather than run with
// deprecation silently off.
func (wc *WhatsAppConnector) startDeprecated(ctx context.Context) error {
	log := wc.Bridge.Log.With().Str("db_section", "homestacks_deprecate").Logger()
	db := wc.Bridge.DB.Child(deprecateVersionTable, deprecateUpgrades, dbutil.ZeroLogger(log))
	if err := db.Upgrade(ctx); err != nil {
		return err
	}
	store := &deprecateStore{db: db, bridgeID: wc.Bridge.ID}
	chats, err := store.loadChats(ctx)
	if err != nil {
		return err
	}
	deprecated.lock.Lock()
	deprecated.store, deprecated.chats = store, chats
	deprecated.lock.Unlock()
	if wc.Bridge.Background {
		return nil
	}
	go func() {
		ctx := log.WithContext(wc.Bridge.BackgroundCtx)
		ticker := time.NewTicker(deprecatedReleaseInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				wc.releaseDueDeprecated(ctx, time.Now())
			}
		}
	}()
	return nil
}
