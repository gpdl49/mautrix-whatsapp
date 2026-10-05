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
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"
	"go.mau.fi/util/ptr"
	"go.mau.fi/util/random"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

// This file builds the call-back link that the homestacks call auto-reply
// hands a WhatsApp caller. See HOMESTACKS.md and, for why it is shaped this
// way, docs/matrix-call-links.md in the homestacks repo.
//
// Element Call never creates a room from a link; it only joins a room whose ID
// is already in the URL. So a link is only useful if a real Matrix room exists
// behind it, which is what this file does: one throwaway room per call, joined
// by the user so the ring reaches them, discarded afterwards.

// Element Call reads these from the URL fragment.
const (
	callLinkFragmentPath = "/room/#/callback"
	// Element Call emits the m.rtc.notification itself when the caller joins,
	// so the bridge needs no ring code of its own and the user is only rung
	// when there is a live call to answer.
	callLinkNotificationType = "ring"
)

// Publishing call membership is a state event, so a caller who has just joined
// a fresh room cannot write it: preset public_chat gives state_default 50 and
// users_default 0. Element Call then joins, is refused, and tears the call down
// within milliseconds -- which looks exactly like a media failure from the
// browser. Both names are needed: current clients write the legacy
// org.matrix.msc3401.call.member, and granting only m.rtc.member looks correct
// while changing nothing.
var callRoomMemberEvents = []string{
	"m.rtc.member",
	"org.matrix.msc3401.call.member",
}

// createCallRoom makes the room a caller will be sent to and returns its ID
// along with the link. The user is joined rather than invited: the ring only
// reaches a joined member, and requiring the user to accept an invite per call
// would defeat the point.
func (wa *WhatsAppClient) createCallRoom(ctx context.Context, name string) (id.RoomID, string, error) {
	cfg := &wa.Main.Config.CallAutoReply
	bot := wa.Main.Bridge.Bot

	pls := &event.PowerLevelsEventContent{Events: map[string]int{}}
	for _, evtType := range callRoomMemberEvents {
		pls.Events[evtType] = 0
	}

	// Deliberately unencrypted. Element Call's own `password` gives the media
	// end-to-end encryption independently, and a throwaway account on the guest
	// homeserver cannot participate in room encryption anyway.
	roomID, err := bot.CreateRoom(ctx, &mautrix.ReqCreateRoom{
		Name:       name,
		Preset:     "public_chat",
		Visibility: "private",
		InitialState: []*event.Event{{
			Type:     event.StateHistoryVisibility,
			StateKey: ptr.Ptr(""),
			Content: event.Content{Parsed: &event.HistoryVisibilityEventContent{
				HistoryVisibility: event.HistoryVisibilityJoined,
			}},
		}},
		PowerLevelOverride: pls,
	})
	if err != nil {
		return "", "", fmt.Errorf("failed to create call room: %w", err)
	}

	// Join the user through their double puppet so the ring reaches them. If
	// double puppeting is not set up we fall back to an invite, which still
	// works but needs a tap before the call can ring.
	if dp := wa.UserLogin.User.DoublePuppet(ctx); dp != nil {
		if err := dp.EnsureJoined(ctx, roomID); err != nil {
			zerolog.Ctx(ctx).Warn().Err(err).
				Msg("Failed to join the user to the call room; falling back to an invite")
			_ = bot.EnsureInvited(ctx, roomID, wa.UserLogin.UserMXID)
		}
	} else {
		zerolog.Ctx(ctx).Warn().
			Msg("No double puppet for this login; inviting instead of joining, so the call will not ring until the invite is accepted")
		_ = bot.EnsureInvited(ctx, roomID, wa.UserLogin.UserMXID)
	}

	return roomID, cfg.buildCallLink(roomID), nil
}

// buildCallLink assembles the Element Call URL for a room. Everything Element
// Call needs lives in the fragment, after the `#`.
func (c *CallAutoReplyConfig) buildCallLink(roomID id.RoomID) string {
	q := url.Values{}
	q.Set("roomId", string(roomID))
	// Media encryption key. Element Call derives the call's E2EE from this, so
	// it must be unguessable: the link is the only credential a caller has.
	q.Set("password", random.String(24))
	q.Set("sendNotificationType", callLinkNotificationType)
	if len(c.ViaServers) > 0 {
		q.Set("viaServers", strings.Join(c.ViaServers, ","))
	}
	// Without this Element Call registers the caller on its own default guest
	// server, which federates with nothing, and the caller can never reach the
	// room. This parameter is what makes the whole scheme work.
	if c.GuestHomeserverURL != "" {
		q.Set("homeserver", c.GuestHomeserverURL)
	}
	return c.CallLinkBaseURL + callLinkFragmentPath + "?" + q.Encode()
}

// callRoomRef is a call room as handed to a caller.
type callRoomRef struct {
	RoomID id.RoomID
	Link   string
	// ExpiresAt is when the room is cleaned up. Zero means never.
	ExpiresAt time.Time
}

// callRoomRecord is a call room awaiting cleanup. It is stored in the bridge
// database (callstate.go) so that a restart does not strand it.
type callRoomRecord struct {
	RoomID    id.RoomID
	LoginID   string
	UserMXID  id.UserID
	CallerKey string // empty for `!wa call` rooms
	ExpiresAt time.Time
}

// createTrackedCallRoom creates a call room and, if room_ttl is set, records
// it for cleanup. callerKey ties the room to an incoming caller; pass "" for
// rooms that belong to no caller.
func (wa *WhatsAppClient) createTrackedCallRoom(ctx context.Context, name, callerKey string) (callRoomRef, error) {
	roomID, link, err := wa.createCallRoom(ctx, name)
	if err != nil {
		return callRoomRef{}, err
	}
	ref := callRoomRef{RoomID: roomID, Link: link}
	if ttl := wa.Main.Config.CallAutoReply.RoomTTL; ttl > 0 {
		ref.ExpiresAt = time.Now().Add(ttl)
		wa.Main.trackCallRoom(ctx, callRoomRecord{
			RoomID:    roomID,
			LoginID:   string(wa.UserLogin.ID),
			UserMXID:  wa.UserLogin.UserMXID,
			CallerKey: callerKey,
			ExpiresAt: ref.ExpiresAt,
		})
	}
	return ref, nil
}

// callRoomTimers holds a cancel func per scheduled cleanup, so that a room
// cleaned up early (its text was never sent) does not get cleaned up twice.
var callRoomTimers sync.Map // id.RoomID -> context.CancelFunc

// trackCallRoom persists a call room and schedules its cleanup.
func (wc *WhatsAppConnector) trackCallRoom(ctx context.Context, rec callRoomRecord) {
	if err := callStore.insertRoom(ctx, rec); err != nil {
		zerolog.Ctx(ctx).Err(err).Stringer("room_id", rec.RoomID).
			Msg("Failed to store call room; it will not be cleaned up if the bridge restarts first")
	}
	wc.scheduleCallRoomCleanup(rec)
}

// scheduleCallRoomCleanup makes the bridge bot and the user leave the room
// once it expires, so call rooms do not accumulate. Rooms still pending when
// the bridge stops are picked up again by startCallState.
func (wc *WhatsAppConnector) scheduleCallRoomCleanup(rec callRoomRecord) {
	ctx, cancel := context.WithCancel(wc.Bridge.BackgroundCtx)
	if prev, loaded := callRoomTimers.Swap(rec.RoomID, cancel); loaded {
		prev.(context.CancelFunc)()
	}
	go func() {
		timer := time.NewTimer(time.Until(rec.ExpiresAt))
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-ctx.Done():
			return
		}
		wc.cleanupCallRoom(ctx, rec.RoomID, rec.UserMXID)
	}()
}

// cleanupCallRoom removes the user (through their double puppet) and the
// bridge bot from a call room and forgets it. If the homeserver could not be
// reached the record is kept and retried at the next startup.
func (wc *WhatsAppConnector) cleanupCallRoom(ctx context.Context, roomID id.RoomID, userMXID id.UserID) {
	if cancel, ok := callRoomTimers.LoadAndDelete(roomID); ok {
		cancel.(context.CancelFunc)()
	}
	// The timer's own context was just cancelled; do the work on a live one.
	ctx = context.WithoutCancel(ctx)
	log := wc.Bridge.Log.With().
		Str("action", "call room cleanup").
		Stringer("room_id", roomID).
		Logger()
	if userMXID != "" {
		if user, err := wc.Bridge.GetExistingUserByMXID(ctx, userMXID); err != nil {
			log.Debug().Err(err).Msg("Failed to look up the call room's user")
		} else if user != nil {
			if dp := user.DoublePuppet(ctx); dp != nil {
				if err = dp.DeleteRoom(ctx, roomID, false); err != nil {
					log.Debug().Err(err).Msg("Failed to remove the user from the expired call room")
				}
			}
		}
	}
	if err := wc.Bridge.Bot.DeleteRoom(ctx, roomID, false); err != nil && !isPermanentMatrixError(err) {
		log.Warn().Err(err).Msg("Failed to clean up the call room; will retry at the next startup")
		return
	} else if err != nil {
		log.Debug().Err(err).Msg("Call room is already gone")
	} else {
		log.Debug().Msg("Cleaned up call room")
	}
	if err := callStore.deleteRoom(ctx, roomID); err != nil {
		log.Err(err).Msg("Failed to forget cleaned up call room")
	}
}

// isPermanentMatrixError reports whether retrying err is pointless: the
// homeserver answered, and said no (e.g. the bot is no longer in the room).
func isPermanentMatrixError(err error) bool {
	var httpErr mautrix.HTTPError
	return errors.As(err, &httpErr) && httpErr.Response != nil &&
		httpErr.Response.StatusCode >= 400 && httpErr.Response.StatusCode < 500
}
