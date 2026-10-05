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
	"time"

	"github.com/rs/zerolog"
	"go.mau.fi/util/dbutil"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/id"
)

// This file persists the homestacks call auto-reply state (see HOMESTACKS.md)
// in the bridge database, so that a restart neither resets callers' cooldowns
// nor strands call rooms that were waiting for their room_ttl cleanup.
//
// The tables are the fork's own and are versioned in their own version table,
// so upstream's whatsapp_version migrations never see or clash with them.

const callStateVersionTable = "homestacks_callstate_version"

var callStateUpgrades = dbutil.BuildUpgradeTable().
	WithRaw(0, 1, 0, "Add homestacks call auto-reply state", dbutil.TxnModeOn, func(ctx context.Context, db *dbutil.Database) error {
		for _, q := range []string{
			`CREATE TABLE homestacks_call_reply (
				bridge_id       TEXT   NOT NULL,
				login_id        TEXT   NOT NULL,
				caller_key      TEXT   NOT NULL,
				last_reply      BIGINT NOT NULL,
				room_id         TEXT   NOT NULL,
				link            TEXT   NOT NULL,
				room_expires_at BIGINT NOT NULL,
				expires_at      BIGINT NOT NULL,
				PRIMARY KEY (bridge_id, login_id, caller_key)
			)`,
			`CREATE TABLE homestacks_call_room (
				bridge_id  TEXT   NOT NULL,
				room_id    TEXT   NOT NULL,
				login_id   TEXT   NOT NULL,
				user_mxid  TEXT   NOT NULL,
				caller_key TEXT   NOT NULL,
				expires_at BIGINT NOT NULL,
				PRIMARY KEY (bridge_id, room_id)
			)`,
		} {
			if _, err := db.Exec(ctx, q); err != nil {
				return err
			}
		}
		return nil
	}).
	Finish()

// callStateStore is the database side of the call auto-reply state. All
// methods are no-ops on a nil store, which is what tests and a bridge whose
// state failed to load get.
type callStateStore struct {
	db       *dbutil.Database
	bridgeID networkid.BridgeID
}

// callStore is set by startCallState. Like callReplies it is shared by every
// login of the (single) connector in this process.
var callStore *callStateStore

// startCallState upgrades the fork's tables, loads callers' cooldowns, and
// sweeps call rooms: expired ones are cleaned up, live ones rescheduled. Only
// a failed schema upgrade is returned as an error.
func (wc *WhatsAppConnector) startCallState(ctx context.Context) error {
	log := wc.Bridge.Log.With().Str("db_section", "homestacks_callstate").Logger()
	db := wc.Bridge.DB.Child(callStateVersionTable, callStateUpgrades, dbutil.ZeroLogger(log))
	if err := db.Upgrade(ctx); err != nil {
		return err
	}
	store := &callStateStore{db: db, bridgeID: wc.Bridge.ID}
	callStore = store
	if wc.Bridge.Background {
		return nil
	}

	now := time.Now()
	// Failing to load is not worth refusing to start the bridge over: the
	// worst case is one extra text per caller and rooms left for a later sweep.
	callers, err := store.loadCallers(ctx, now)
	if err != nil {
		log.Err(err).Msg("Failed to load call auto-reply cooldowns")
	}
	for key, e := range callers {
		callReplies.load(key, e)
	}
	rooms, err := store.loadRooms(ctx)
	if err != nil {
		log.Err(err).Msg("Failed to load pending call rooms")
	}
	var expired []callRoomRecord
	for _, rec := range rooms {
		if rec.ExpiresAt.After(now) {
			wc.scheduleCallRoomCleanup(rec)
		} else {
			expired = append(expired, rec)
		}
	}
	log.Debug().
		Int("callers", len(callers)).
		Int("live_rooms", len(rooms)-len(expired)).
		Int("expired_rooms", len(expired)).
		Msg("Loaded call auto-reply state")
	if len(expired) > 0 {
		// Matrix requests; keep them off the startup path.
		go func() {
			ctx := wc.Bridge.BackgroundCtx
			for _, rec := range expired {
				wc.cleanupCallRoom(ctx, rec.RoomID, rec.UserMXID)
			}
		}()
	}
	return nil
}

// saveCaller persists a tracker entry. Best effort: on failure the in-memory
// tracker still works, only a restart would forget the cooldown.
func (wc *WhatsAppConnector) saveCaller(ctx context.Context, key callerKey, e callerEntry) {
	if err := callStore.upsertCaller(ctx, key, e); err != nil {
		zerolog.Ctx(ctx).Err(err).Msg("Failed to store call auto-reply cooldown")
	}
}

func unixMilliOrZero(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixMilli()
}

func timeFromUnixMilli(ms int64) time.Time {
	if ms == 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms)
}

func (s *callStateStore) upsertCaller(ctx context.Context, key callerKey, e callerEntry) error {
	if s == nil {
		return nil
	}
	_, err := s.db.Exec(ctx, `
		INSERT INTO homestacks_call_reply
			(bridge_id, login_id, caller_key, last_reply, room_id, link, room_expires_at, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (bridge_id, login_id, caller_key) DO UPDATE SET
			last_reply=excluded.last_reply, room_id=excluded.room_id, link=excluded.link,
			room_expires_at=excluded.room_expires_at, expires_at=excluded.expires_at
	`, s.bridgeID, key.LoginID, key.Caller, unixMilliOrZero(e.LastReply), e.RoomID, e.Link,
		unixMilliOrZero(e.RoomExpiresAt), unixMilliOrZero(e.ExpiresAt))
	return err
}

// loadCallers drops entries that expired before now and returns the rest.
func (s *callStateStore) loadCallers(ctx context.Context, now time.Time) (map[callerKey]callerEntry, error) {
	out := make(map[callerKey]callerEntry)
	if s == nil {
		return out, nil
	}
	_, err := s.db.Exec(ctx, `DELETE FROM homestacks_call_reply WHERE bridge_id=$1 AND expires_at<=$2`,
		s.bridgeID, now.UnixMilli())
	if err != nil {
		return nil, err
	}
	rows, err := s.db.Query(ctx, `
		SELECT login_id, caller_key, last_reply, room_id, link, room_expires_at, expires_at
		FROM homestacks_call_reply WHERE bridge_id=$1
	`, s.bridgeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var key callerKey
		var e callerEntry
		var lastReply, roomExpires, expires int64
		if err = rows.Scan(&key.LoginID, &key.Caller, &lastReply, &e.RoomID, &e.Link, &roomExpires, &expires); err != nil {
			return nil, err
		}
		e.LastReply = timeFromUnixMilli(lastReply)
		e.RoomExpiresAt = timeFromUnixMilli(roomExpires)
		e.ExpiresAt = timeFromUnixMilli(expires)
		out[key] = e
	}
	return out, rows.Err()
}

func (s *callStateStore) insertRoom(ctx context.Context, rec callRoomRecord) error {
	if s == nil {
		return nil
	}
	_, err := s.db.Exec(ctx, `
		INSERT INTO homestacks_call_room (bridge_id, room_id, login_id, user_mxid, caller_key, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (bridge_id, room_id) DO UPDATE SET expires_at=excluded.expires_at
	`, s.bridgeID, rec.RoomID, rec.LoginID, rec.UserMXID, rec.CallerKey, rec.ExpiresAt.UnixMilli())
	return err
}

func (s *callStateStore) deleteRoom(ctx context.Context, roomID id.RoomID) error {
	if s == nil {
		return nil
	}
	_, err := s.db.Exec(ctx, `DELETE FROM homestacks_call_room WHERE bridge_id=$1 AND room_id=$2`, s.bridgeID, roomID)
	return err
}

func (s *callStateStore) loadRooms(ctx context.Context) ([]callRoomRecord, error) {
	if s == nil {
		return nil, nil
	}
	rows, err := s.db.Query(ctx, `
		SELECT room_id, login_id, user_mxid, caller_key, expires_at
		FROM homestacks_call_room WHERE bridge_id=$1
	`, s.bridgeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []callRoomRecord
	for rows.Next() {
		var rec callRoomRecord
		var expires int64
		if err = rows.Scan(&rec.RoomID, &rec.LoginID, &rec.UserMXID, &rec.CallerKey, &expires); err != nil {
			return nil, err
		}
		rec.ExpiresAt = time.UnixMilli(expires)
		out = append(out, rec)
	}
	return out, rows.Err()
}
