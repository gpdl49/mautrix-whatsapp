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
	"slices"
	"strings"

	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/networkid"

	"go.mau.fi/mautrix-whatsapp/pkg/waid"
)

// This file implements the homestacks per-chat send limit: a self-imposed cap
// on how many messages the user may send from Matrix relative to what the
// other side sends. A message over the limit is refused before it is converted
// or uploaded, and the bridge's usual failure notice tells the user why. The
// counts come from the bridge's own message table, so messages sent from the
// phone count too (they just can't be stopped), and nothing is lost on restart.

// rateLimitWindow bounds how far back the history is replayed. The credit
// starts full at the oldest message read, so a longer window only matters for
// chats with long unbroken runs from one side.
const rateLimitWindow = 200

// rateLimitState is the result of replaying a chat's history against its
// settings. Credit is in units of one own message: in ratio mode a message
// from the other side is worth Mine/Theirs of one, so the arithmetic is done
// in 1/Theirs steps to stay exact.
type rateLimitState struct {
	// Available is how many messages could be sent right now.
	Available int
	// Unanswered is the number of own messages since the other side last wrote.
	Unanswered int
	// RepliesNeeded is how many more messages from the other side unlock the
	// next own message; zero when one can be sent now.
	RepliesNeeded int
}

// replayRateLimit computes the state after a history of messages, given
// oldest first as "was this message mine". Own messages may drive the credit
// negative -- ones sent from the phone bypass the check but still count.
func replayRateLimit(s *waid.RateLimitSettings, fromMe []bool) rateLimitState {
	unit, gain := 1, s.Burst
	if s.Mode == waid.RateLimitModeRatio {
		unit, gain = s.Theirs, s.Mine
	}
	capacity := s.Burst * unit
	credit := capacity
	var st rateLimitState
	for _, mine := range fromMe {
		if mine {
			credit -= unit
			st.Unanswered++
			continue
		}
		credit = min(capacity, credit+gain)
		st.Unanswered = 0
	}
	if credit >= unit {
		st.Available = credit / unit
	} else if s.Mode != waid.RateLimitModeRatio {
		// Any one reply restores the full burst, however far overdrawn.
		st.RepliesNeeded = 1
	} else {
		st.RepliesNeeded = (unit - credit + gain - 1) / gain
	}
	return st
}

// describeRateLimit is the human form of a chat's settings.
func describeRateLimit(s *waid.RateLimitSettings) string {
	if s.Mode == waid.RateLimitModeRatio {
		return fmt.Sprintf("%d of yours per %d of theirs, at most %d in a row", s.Mine, s.Theirs, s.Burst)
	}
	if s.Burst == 1 {
		return "one message per reply"
	}
	return fmt.Sprintf("at most %d messages without a reply", s.Burst)
}

// rateLimitRefusal is the reason shown in "Your message was not bridged: ...".
func rateLimitRefusal(s *waid.RateLimitSettings, st rateLimitState) string {
	if s.Mode == waid.RateLimitModeRatio {
		more := "message"
		if st.RepliesNeeded != 1 {
			more = "messages"
		}
		return fmt.Sprintf("rate limit for this chat (%s). Wait for %d more %s from them.", describeRateLimit(s), st.RepliesNeeded, more)
	}
	return fmt.Sprintf("rate limit for this chat (%s). Wait for a reply; you have %d unanswered.", describeRateLimit(s), st.Unanswered)
}

func portalRateLimit(portal *bridgev2.Portal) *waid.RateLimitSettings {
	meta, ok := portal.Metadata.(*waid.PortalMetadata)
	if !ok {
		return nil
	}
	return meta.RateLimit
}

// readRateLimitState reads the chat's recent history and replays it.
func (wa *WhatsAppClient) readRateLimitState(ctx context.Context, portal *bridgev2.Portal, s *waid.RateLimitSettings) (rateLimitState, error) {
	msgs, err := wa.Main.Bridge.DB.Message.GetLastNInPortal(ctx, portal.PortalKey, rateLimitWindow)
	if err != nil {
		return rateLimitState{}, err
	}
	own := []networkid.UserID{waid.MakeUserID(wa.JID.ToNonAD())}
	if lid := wa.GetLID(); !lid.IsEmpty() {
		own = append(own, waid.MakeUserID(lid.ToNonAD()))
	}
	seen := make(map[networkid.MessageID]struct{}, len(msgs))
	fromMe := make([]bool, 0, len(msgs))
	// Newest first from the database; collect, then reverse.
	for _, msg := range msgs {
		if msg.Timestamp.Before(s.EnabledAt.Time) {
			break
		}
		if msg.SenderID == "" || strings.HasPrefix(string(msg.MXID), "~fake:") {
			continue
		}
		if _, dup := seen[msg.ID]; dup {
			continue
		}
		seen[msg.ID] = struct{}{}
		fromMe = append(fromMe, slices.Contains(own, msg.SenderID))
	}
	slices.Reverse(fromMe)
	return replayRateLimit(s, fromMe), nil
}

// checkSendLimit refuses a Matrix message that would exceed the chat's limit.
// Called from HandleMatrixMessage and HandleMatrixPollStart; returns nil for
// chats without a limit.
func (wa *WhatsAppClient) checkSendLimit(ctx context.Context, portal *bridgev2.Portal) error {
	s := portalRateLimit(portal)
	if s == nil {
		return nil
	}
	st, err := wa.readRateLimitState(ctx, portal, s)
	if err != nil {
		// Fail open: a database hiccup should not silently eat messages.
		zerolog.Ctx(ctx).Err(err).Msg("Failed to read history for send limit, allowing message")
		return nil
	}
	if st.Available > 0 {
		return nil
	}
	return bridgev2.WrapErrorInStatus(errors.New(rateLimitRefusal(s, st))).
		WithErrorAsMessage().WithIsCertain(true).WithSendNotice(true)
}
