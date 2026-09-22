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
	"fmt"
	"net/url"
	"strings"
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
func (wa *WhatsAppClient) createCallRoom(ctx context.Context, callerName string) (id.RoomID, string, error) {
	cfg := &wa.Main.Config.CallAutoReply
	bot := wa.Main.Bridge.Bot

	pls := &event.PowerLevelsEventContent{Events: map[string]int{}}
	for _, evtType := range callRoomMemberEvents {
		pls.Events[evtType] = 0
	}

	name := "Incoming WhatsApp call"
	if callerName != "" {
		name = "WhatsApp call from " + callerName
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

// scheduleCallRoomCleanup makes the bridge bot and the user leave the room
// after the configured TTL, so call rooms do not accumulate. Best effort: if
// the bridge restarts first the room is simply left behind, which is untidy
// rather than harmful.
func (wa *WhatsAppClient) scheduleCallRoomCleanup(roomID id.RoomID, ttl time.Duration) {
	if ttl <= 0 {
		return
	}
	go func() {
		ctx := wa.Main.Bridge.BackgroundCtx
		select {
		case <-time.After(ttl):
		case <-ctx.Done():
			return
		}
		log := wa.UserLogin.Log.With().
			Str("action", "call room cleanup").
			Stringer("room_id", roomID).
			Logger()
		if dp := wa.UserLogin.User.DoublePuppet(ctx); dp != nil {
			if err := dp.DeleteRoom(ctx, roomID, false); err != nil {
				log.Debug().Err(err).Msg("Failed to remove the user from the expired call room")
			}
		}
		if err := wa.Main.Bridge.Bot.DeleteRoom(ctx, roomID, false); err != nil {
			log.Debug().Err(err).Msg("Failed to clean up the expired call room")
		} else {
			log.Debug().Msg("Cleaned up expired call room")
		}
	}()
}
