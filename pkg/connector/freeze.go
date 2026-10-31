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
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/rs/zerolog"
	"go.mau.fi/util/dbutil"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/networkid"
)

// This file implements the homestacks frozen chats (`!wa freeze`): WhatsApp
// messages in a frozen chat are not bridged to Matrix but stored, and
// replayed through the normal message handler when the chat is unfrozen, so
// they arrive with their original timestamps and reactions, edits and
// deletes made meanwhile still apply. Matrix messages sent into a frozen chat
// are refused.
//
// Everything is keyed by portal key, the same key the message would be
// queued under, so a chat frozen by number before its room exists still
// catches its first message.

const freezeVersionTable = "homestacks_freeze_version"

var freezeUpgrades = dbutil.BuildUpgradeTable().
	WithRaw(0, 1, 0, "Add homestacks frozen chats", dbutil.TxnModeOn, func(ctx context.Context, db *dbutil.Database) error {
		if _, err := db.Exec(ctx, `CREATE TABLE homestacks_frozen_chat (
			bridge_id       TEXT   NOT NULL,
			portal_id       TEXT   NOT NULL,
			portal_receiver TEXT   NOT NULL,
			frozen_at       BIGINT NOT NULL,
			PRIMARY KEY (bridge_id, portal_id, portal_receiver)
		)`); err != nil {
			return err
		}
		_, err := db.Exec(ctx, `CREATE TABLE homestacks_frozen_event (
			bridge_id       TEXT   NOT NULL,
			portal_id       TEXT   NOT NULL,
			portal_receiver TEXT   NOT NULL,
			login_id        TEXT   NOT NULL,
			message_id      TEXT   NOT NULL,
			seq             BIGINT NOT NULL,
			event_json      TEXT   NOT NULL,
			PRIMARY KEY (bridge_id, portal_id, portal_receiver, login_id, message_id)
		)`)
		return err
	}).
	Finish()

type freezeReplayKey struct{}

type frozenRow struct {
	LoginID   networkid.UserLoginID
	MessageID types.MessageID
	Seq       int64
	Event     json.RawMessage
}

type freezeStore struct {
	db       *dbutil.Database
	bridgeID networkid.BridgeID
}

func (s *freezeStore) loadChats(ctx context.Context) (map[networkid.PortalKey]time.Time, error) {
	rows, err := s.db.Query(ctx, `SELECT portal_id, portal_receiver, frozen_at FROM homestacks_frozen_chat WHERE bridge_id=$1`, s.bridgeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[networkid.PortalKey]time.Time)
	for rows.Next() {
		var key networkid.PortalKey
		var at int64
		if err = rows.Scan(&key.ID, &key.Receiver, &at); err != nil {
			return nil, err
		}
		out[key] = time.UnixMilli(at)
	}
	return out, rows.Err()
}

func (s *freezeStore) putChat(ctx context.Context, key networkid.PortalKey, at time.Time) error {
	_, err := s.db.Exec(ctx, `INSERT INTO homestacks_frozen_chat (bridge_id, portal_id, portal_receiver, frozen_at)
		VALUES ($1, $2, $3, $4) ON CONFLICT DO NOTHING`, s.bridgeID, key.ID, key.Receiver, at.UnixMilli())
	return err
}

func (s *freezeStore) deleteChat(ctx context.Context, key networkid.PortalKey) error {
	_, err := s.db.Exec(ctx, `DELETE FROM homestacks_frozen_chat WHERE bridge_id=$1 AND portal_id=$2 AND portal_receiver=$3`,
		s.bridgeID, key.ID, key.Receiver)
	return err
}

// putEvent stores a held WhatsApp message. A redelivery of one already held
// is ignored.
func (s *freezeStore) putEvent(ctx context.Context, key networkid.PortalKey, r *frozenRow) error {
	_, err := s.db.Exec(ctx, `INSERT INTO homestacks_frozen_event
		(bridge_id, portal_id, portal_receiver, login_id, message_id, seq, event_json)
		VALUES ($1, $2, $3, $4, $5, $6, $7) ON CONFLICT DO NOTHING`,
		s.bridgeID, key.ID, key.Receiver, r.LoginID, r.MessageID, r.Seq, string(r.Event))
	return err
}

func (s *freezeStore) deleteEvent(ctx context.Context, key networkid.PortalKey, r *frozenRow) error {
	_, err := s.db.Exec(ctx, `DELETE FROM homestacks_frozen_event
		WHERE bridge_id=$1 AND portal_id=$2 AND portal_receiver=$3 AND login_id=$4 AND message_id=$5`,
		s.bridgeID, key.ID, key.Receiver, r.LoginID, r.MessageID)
	return err
}

func (s *freezeStore) listEvents(ctx context.Context, key networkid.PortalKey) ([]*frozenRow, error) {
	rows, err := s.db.Query(ctx, `SELECT login_id, message_id, seq, event_json FROM homestacks_frozen_event
		WHERE bridge_id=$1 AND portal_id=$2 AND portal_receiver=$3 ORDER BY seq`, s.bridgeID, key.ID, key.Receiver)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*frozenRow
	for rows.Next() {
		var r frozenRow
		var evt string
		if err = rows.Scan(&r.LoginID, &r.MessageID, &r.Seq, &evt); err != nil {
			return nil, err
		}
		r.Event = json.RawMessage(evt)
		out = append(out, &r)
	}
	return out, rows.Err()
}

func (s *freezeStore) countEvents(ctx context.Context, key networkid.PortalKey) (n int, err error) {
	err = s.db.QueryRow(ctx, `SELECT COUNT(*) FROM homestacks_frozen_event
		WHERE bridge_id=$1 AND portal_id=$2 AND portal_receiver=$3`, s.bridgeID, key.ID, key.Receiver).Scan(&n)
	return
}

// freezeState is the in-memory copy of the frozen chat list, so the check on
// every incoming message doesn't touch the database. lock is only held for
// single operations, never across a replay, so a long unfreeze doesn't stall
// other chats; the chat stays frozen while it is replayed and is unfrozen by
// the locked check that finds nothing left.
type freezeState struct {
	lock    sync.Mutex
	store   *freezeStore
	chats   map[networkid.PortalKey]time.Time
	thawing map[networkid.PortalKey]bool
	lastSeq int64
}

// frozen is set up by startFreeze; a nil store disables freezing entirely.
var frozen = &freezeState{
	chats:   make(map[networkid.PortalKey]time.Time),
	thawing: make(map[networkid.PortalKey]bool),
}

func (fs *freezeState) since(key networkid.PortalKey) (time.Time, bool) {
	fs.lock.Lock()
	defer fs.lock.Unlock()
	at, ok := fs.chats[key]
	return at, ok
}

func (fs *freezeState) list() map[networkid.PortalKey]time.Time {
	fs.lock.Lock()
	defer fs.lock.Unlock()
	out := make(map[networkid.PortalKey]time.Time, len(fs.chats))
	for k, v := range fs.chats {
		out[k] = v
	}
	return out
}

// nextSeq orders held messages by arrival, even several in one nanosecond.
// Callers hold lock.
func (fs *freezeState) nextSeq(now time.Time) int64 {
	seq := now.UnixNano()
	if seq <= fs.lastSeq {
		seq = fs.lastSeq + 1
	}
	fs.lastSeq = seq
	return seq
}

func (fs *freezeState) freeze(ctx context.Context, key networkid.PortalKey, now time.Time) (since time.Time, already bool, err error) {
	fs.lock.Lock()
	defer fs.lock.Unlock()
	if at, ok := fs.chats[key]; ok {
		return at, true, nil
	}
	if err = fs.store.putChat(ctx, key, now); err != nil {
		return time.Time{}, false, err
	}
	fs.chats[key] = now
	return now, false, nil
}

// frozenEvent is what is kept of a whatsmeow message to handle it again. The
// raw message is kept rather than the unwrapped one, since the handler
// decrypts and rewrites the unwrapped copy in place.
type frozenEvent struct {
	Info                 types.MessageInfo             `json:"info"`
	Raw                  []byte                        `json:"raw"`
	UnavailableRequestID types.MessageID               `json:"unavailable_request_id,omitempty"`
	RetryCount           int                           `json:"retry_count,omitempty"`
	NewsletterMeta       *events.NewsletterMessageMeta `json:"newsletter_meta,omitempty"`
}

func encodeFrozenEvent(evt *events.Message) (json.RawMessage, error) {
	raw := evt.RawMessage
	if raw == nil {
		raw = evt.Message
	}
	data, err := proto.Marshal(raw)
	if err != nil {
		return nil, err
	}
	return json.Marshal(&frozenEvent{
		Info:                 evt.Info,
		Raw:                  data,
		UnavailableRequestID: evt.UnavailableRequestID,
		RetryCount:           evt.RetryCount,
		NewsletterMeta:       evt.NewsletterMeta,
	})
}

func decodeFrozenEvent(data json.RawMessage) (*events.Message, error) {
	var fe frozenEvent
	if err := json.Unmarshal(data, &fe); err != nil {
		return nil, err
	}
	var raw waE2E.Message
	if err := proto.Unmarshal(fe.Raw, &raw); err != nil {
		return nil, err
	}
	evt := &events.Message{
		Info:                 fe.Info,
		RawMessage:           &raw,
		UnavailableRequestID: fe.UnavailableRequestID,
		RetryCount:           fe.RetryCount,
		NewsletterMeta:       fe.NewsletterMeta,
	}
	return evt.UnwrapRaw(), nil
}

// checkFrozen holds a WhatsApp message whose chat is frozen. Called from
// handleWAMessage once the alt JIDs are known (they decide the portal key);
// returns true if the message was held and must not be bridged now.
func (wa *WhatsAppClient) checkFrozen(ctx context.Context, evt *events.Message) bool {
	if ctx.Value(freezeReplayKey{}) != nil {
		return false
	}
	key := (&MessageInfoWrapper{Info: evt.Info, wa: wa}).GetPortalKey()
	frozen.lock.Lock()
	defer frozen.lock.Unlock()
	if _, ok := frozen.chats[key]; !ok || frozen.store == nil {
		return false
	}
	log := zerolog.Ctx(ctx).With().Str("message_id", evt.Info.ID).Str("portal_id", string(key.ID)).Logger()
	data, err := encodeFrozenEvent(evt)
	if err == nil {
		err = frozen.store.putEvent(ctx, key, &frozenRow{
			LoginID: wa.UserLogin.ID, MessageID: evt.Info.ID, Seq: frozen.nextSeq(time.Now()), Event: data,
		})
	}
	if err != nil {
		// Fail open: bridging a message early beats losing it.
		log.Err(err).Msg("Failed to hold message in frozen chat, bridging it now")
		return false
	}
	log.Debug().Msg("Held message in frozen chat")
	return true
}

// checkFrozenSend refuses a Matrix message sent into a frozen chat. Called
// from HandleMatrixMessage and HandleMatrixPollStart.
func (wa *WhatsAppClient) checkFrozenSend(portal *bridgev2.Portal) error {
	since, ok := frozen.since(portal.PortalKey)
	if !ok {
		return nil
	}
	return bridgev2.WrapErrorInStatus(fmt.Errorf("this chat is frozen (since %s); `%s unfreeze` it first",
		since.In(userLocation(wa.UserLogin)).Format("2006-01-02 15:04 MST"), wa.Main.Bridge.Config.CommandPrefix)).
		WithErrorAsMessage().WithIsCertain(true).WithSendNotice(true)
}

var errFrozenLoginOffline = errors.New("not connected")

// frozenClient returns the connected client of the login a message was held
// for. A login that no longer exists returns nil and no error, so its
// messages are dropped.
func (wc *WhatsAppConnector) frozenClient(ctx context.Context, loginID networkid.UserLoginID) (*WhatsAppClient, error) {
	login := wc.Bridge.GetCachedUserLoginByID(loginID)
	if login == nil {
		var err error
		if login, err = wc.Bridge.GetExistingUserLoginByID(ctx, loginID); err != nil {
			return nil, err
		} else if login == nil {
			return nil, nil
		}
	}
	wa, ok := login.Client.(*WhatsAppClient)
	if !ok || !wa.IsLoggedIn() || wa.Client == nil || !wa.Client.IsConnected() {
		return nil, fmt.Errorf("WhatsApp account %s is %w", login.RemoteName, errFrozenLoginOffline)
	}
	return wa, nil
}

var errUnfreezeRunning = errors.New("an unfreeze of this chat is already running")

// nextBatch returns the chat's held messages, or, if there are none left,
// unfreezes it and reports done. Doing both under the lock means a message
// can't slip in between the last replay and the unfreeze.
func (fs *freezeState) nextBatch(ctx context.Context, key networkid.PortalKey) (rows []*frozenRow, done bool, err error) {
	fs.lock.Lock()
	defer fs.lock.Unlock()
	if _, ok := fs.chats[key]; !ok {
		return nil, true, nil
	}
	if rows, err = fs.store.listEvents(ctx, key); err != nil || len(rows) > 0 {
		return rows, false, err
	}
	if err = fs.store.deleteChat(ctx, key); err != nil {
		return nil, false, err
	}
	delete(fs.chats, key)
	return nil, true, nil
}

func (fs *freezeState) startThaw(key networkid.PortalKey) bool {
	fs.lock.Lock()
	defer fs.lock.Unlock()
	if fs.thawing[key] {
		return false
	}
	fs.thawing[key] = true
	return true
}

func (fs *freezeState) endThaw(key networkid.PortalKey) {
	fs.lock.Lock()
	defer fs.lock.Unlock()
	delete(fs.thawing, key)
}

// unfreeze replays the chat's held messages in order and then unfreezes it.
// Messages arriving meanwhile are held too and replayed in the next batch.
// If one can't be replayed, the chat stays frozen with it and everything
// after it still held, and the error says why.
func (wc *WhatsAppConnector) unfreeze(ctx context.Context, key networkid.PortalKey) (replayed, dropped int, err error) {
	if !frozen.startThaw(key) {
		return 0, 0, errUnfreezeRunning
	}
	defer frozen.endThaw(key)
	log := zerolog.Ctx(ctx).With().Str("portal_id", string(key.ID)).Logger()
	for {
		rows, done, err := frozen.nextBatch(ctx, key)
		if err != nil {
			return replayed, dropped, err
		} else if done {
			log.Info().Int("replayed", replayed).Int("dropped", dropped).Msg("Unfroze chat")
			return replayed, dropped, nil
		}
		for _, r := range rows {
			wa, err := wc.frozenClient(ctx, r.LoginID)
			if err != nil {
				return replayed, dropped, err
			}
			evt, decodeErr := decodeFrozenEvent(r.Event)
			if wa == nil || decodeErr != nil {
				log.Warn().Err(decodeErr).Str("message_id", r.MessageID).Msg("Dropping held message of frozen chat")
				if err = frozen.store.deleteEvent(ctx, key, r); err != nil {
					return replayed, dropped, err
				}
				dropped++
				continue
			}
			replayCtx := wa.UserLogin.Log.WithContext(context.WithValue(wc.Bridge.BackgroundCtx, freezeReplayKey{}, true))
			if !wa.handleWAMessage(replayCtx, evt) {
				return replayed, dropped, fmt.Errorf("message %s could not be bridged", r.MessageID)
			}
			if err = frozen.store.deleteEvent(ctx, key, r); err != nil {
				return replayed, dropped, err
			}
			replayed++
		}
	}
}

// startFreeze upgrades the frozen chat tables and loads the frozen chats.
func (wc *WhatsAppConnector) startFreeze(ctx context.Context) error {
	log := wc.Bridge.Log.With().Str("db_section", "homestacks_freeze").Logger()
	db := wc.Bridge.DB.Child(freezeVersionTable, freezeUpgrades, dbutil.ZeroLogger(log))
	if err := db.Upgrade(ctx); err != nil {
		return err
	}
	store := &freezeStore{db: db, bridgeID: wc.Bridge.ID}
	chats, err := store.loadChats(ctx)
	if err != nil {
		return err
	}
	frozen.lock.Lock()
	defer frozen.lock.Unlock()
	frozen.store, frozen.chats = store, chats
	return nil
}
