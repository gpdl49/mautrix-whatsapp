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
	"html"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/simplevent"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"go.mau.fi/mautrix-whatsapp/pkg/waid"
)

// This file implements the homestacks call auto-reply feature: incoming
// WhatsApp calls are declined, the caller receives a configurable text (with
// an Element Call link) and the Matrix portal gets a notice. It is registered
// as a second whatsmeow event handler next to handleWAEvent so that the
// upstream call handling stays untouched. See HOMESTACKS.md.

// callReplyTracker remembers which calls were already handled and when each
// caller last received an auto-reply, so that CallOffer + CallOfferNotice for
// one call, or a caller retrying three times in a row, produce a single text.
// One instance is shared by all logins; keys are prefixed with the login ID.
type callReplyTracker struct {
	lock      sync.Mutex
	seenCalls map[string]time.Time
	lastReply map[string]time.Time
	lastPrune time.Time
}

const callReplyTrackerRetention = time.Hour

var callReplies = &callReplyTracker{
	seenCalls: make(map[string]time.Time),
	lastReply: make(map[string]time.Time),
}

// markCall records a call and reports whether it is new.
func (t *callReplyTracker) markCall(now time.Time, loginID, callID string) bool {
	t.lock.Lock()
	defer t.lock.Unlock()
	t.pruneLocked(now)
	key := loginID + "/" + callID
	if _, seen := t.seenCalls[key]; seen {
		return false
	}
	t.seenCalls[key] = now
	return true
}

// markReply records that a reply is being sent to caller and reports whether
// the caller is outside the cooldown window.
func (t *callReplyTracker) markReply(now time.Time, loginID, caller string, cooldown time.Duration) bool {
	t.lock.Lock()
	defer t.lock.Unlock()
	key := loginID + "/" + caller
	if last, ok := t.lastReply[key]; ok && cooldown > 0 && now.Sub(last) < cooldown {
		return false
	}
	t.lastReply[key] = now
	return true
}

func (t *callReplyTracker) pruneLocked(now time.Time) {
	if now.Sub(t.lastPrune) < callReplyTrackerRetention/4 {
		return
	}
	t.lastPrune = now
	for key, ts := range t.seenCalls {
		if now.Sub(ts) > callReplyTrackerRetention {
			delete(t.seenCalls, key)
		}
	}
	for key, ts := range t.lastReply {
		if now.Sub(ts) > callReplyTrackerRetention {
			delete(t.lastReply, key)
		}
	}
}

// callAutoReplySettings resolves the effective settings for this login:
// per-login overrides from `!wa call-reply` win over the bridge config.
func (wa *WhatsAppClient) callAutoReplySettings() (enabled bool, message string) {
	cfg := &wa.Main.Config.CallAutoReply
	enabled, message = cfg.Enabled, cfg.Message
	if meta, ok := wa.UserLogin.Metadata.(*waid.UserLoginMetadata); ok && meta.CallAutoReply != nil {
		if meta.CallAutoReply.Enabled != nil {
			enabled = *meta.CallAutoReply.Enabled
		}
		if meta.CallAutoReply.Message != "" {
			message = meta.CallAutoReply.Message
		}
	}
	return
}

// handleCallAutoReply is a whatsmeow event handler. It always returns true:
// the auto-reply is best effort and must never hold up event acknowledgement.
func (wa *WhatsAppClient) handleCallAutoReply(rawEvt any) bool {
	var meta types.BasicCallMeta
	var callType string
	switch evt := rawEvt.(type) {
	case *events.CallOffer:
		meta = evt.BasicCallMeta
	case *events.CallOfferNotice:
		meta = evt.BasicCallMeta
		callType = evt.Media
	default:
		return true
	}
	enabled, message := wa.callAutoReplySettings()
	if !enabled || time.Since(meta.Timestamp) > callEventMaxAge {
		return true
	}
	if !meta.GroupJID.IsEmpty() && !wa.Main.Config.CallAutoReply.IncludeGroupCalls {
		return true
	}
	if !callReplies.markCall(time.Now(), string(wa.UserLogin.ID), meta.CallID) {
		return true
	}
	log := wa.UserLogin.Log.With().
		Str("action", "call auto-reply").
		Str("call_id", meta.CallID).
		Stringer("call_creator", meta.CallCreator).
		Str("call_type", callType).
		Logger()
	// Declining and replying both hit the network; do it off the event loop.
	go wa.autoReplyToCall(log.WithContext(wa.Main.Bridge.BackgroundCtx), meta, callType, message)
	return true
}

func (wa *WhatsAppClient) autoReplyToCall(ctx context.Context, meta types.BasicCallMeta, callType, message string) {
	log := zerolog.Ctx(ctx)
	caller := meta.CallCreator
	if caller.IsEmpty() {
		caller = meta.From
	}
	caller = caller.ToNonAD()
	// The same person may show up under a phone number or a LID depending on
	// the caller's client; keep both so that the contact lookup and the
	// cooldown key are stable.
	pn, lid := caller, meta.CallCreatorAlt.ToNonAD()
	if caller.Server == types.HiddenUserServer {
		pn, lid = lid, caller
		if pn.IsEmpty() {
			pn, _ = wa.GetStore().LIDs.GetPNForLID(ctx, lid)
		}
	} else if lid.IsEmpty() {
		lid, _ = wa.GetStore().LIDs.GetLIDForPN(ctx, pn)
	}
	cooldownKey := pn.String()
	if pn.IsEmpty() {
		cooldownKey = lid.String()
	}
	// Mirror handleWACallStart: DMs are addressed and portal-keyed by the
	// caller's LID when one is known, otherwise by the phone number. Using
	// anything else would create a duplicate portal.
	dm := lid
	if dm.IsEmpty() {
		dm = pn
	}

	if err := wa.Client.RejectCall(ctx, caller, meta.CallID); err != nil {
		log.Err(err).Msg("Failed to reject incoming call")
	} else {
		log.Debug().Msg("Rejected incoming call")
	}

	data := callReplyTemplateData{
		Name:     wa.callerDisplayName(ctx, pn, lid),
		CallType: callType,
	}
	if !pn.IsEmpty() {
		data.Phone = "+" + pn.User
	}
	if data.Name == "" {
		data.Name = data.Phone
	}

	// call-link: create the room the caller will be sent to. Element Call only
	// ever joins a room that already exists, so the link is worthless without
	// this. Everything behind this seam lives in callroom.go and is the part of
	// the fork that is NOT upstreamable -- see "Upstreaming" in HOMESTACKS.md.
	var callRoomID id.RoomID
	if wa.Main.Config.CallAutoReply.CallLinkBaseURL != "" {
		var err error
		callRoomID, data.CallLink, err = wa.createCallRoom(ctx, data.Name)
		if err != nil {
			// Send the text anyway: a caller being told "I can't take WhatsApp
			// calls" without a link is still better than silence.
			log.Err(err).Msg("Failed to create the call room; replying without a link")
		}
	}

	var replyText string
	sent := false
	if callReplies.markReply(time.Now(), string(wa.UserLogin.ID), cooldownKey, wa.Main.Config.CallAutoReply.Cooldown) {
		tmpl, err := parseCallReplyTemplate(message)
		if err != nil {
			log.Err(err).Msg("Invalid call auto-reply template, falling back to bridge config")
			tmpl = wa.Main.Config.CallAutoReply.messageTemplate
		}
		replyText, err = renderCallReply(tmpl, data)
		if err != nil {
			log.Err(err).Msg("Failed to render call auto-reply")
		} else if _, err = wa.Client.SendMessage(ctx, dm, &waE2E.Message{Conversation: proto.String(replyText)}); err != nil {
			log.Err(err).Msg("Failed to send call auto-reply")
		} else {
			sent = true
			log.Info().Msg("Sent call auto-reply")
		}
	} else {
		log.Debug().Msg("Caller is within the auto-reply cooldown, not sending another text")
	}

	chat := meta.GroupJID
	if chat.IsEmpty() {
		chat = dm
	}
	res := wa.UserLogin.QueueRemoteEvent(&simplevent.Message[callReplyNotice]{
		EventMeta: simplevent.EventMeta{
			Type:         bridgev2.RemoteEventMessage,
			PortalKey:    wa.makeWAPortalKey(chat),
			Sender:       bridgev2.EventSender{IsFromMe: true},
			CreatePortal: true,
			Timestamp:    time.Now(),
			StreamOrder:  time.Now().Unix(),
		},
		ID:                 waid.MakeFakeMessageID(chat, dm, "call-reply-"+meta.CallID),
		Data:               callReplyNotice{Data: data, ReplyText: replyText, Sent: sent},
		ConvertMessageFunc: convertCallReplyNotice,
	})
	if !res.Success {
		log.Warn().Msg("Failed to queue call auto-reply notice to Matrix")
	}

	// call-link: call rooms are per-call and disposable; drop this one after the
	// TTL so they do not accumulate one per missed call forever.
	if callRoomID != "" {
		wa.scheduleCallRoomCleanup(callRoomID, wa.Main.Config.CallAutoReply.RoomTTL)
	}
}

// callerDisplayName returns the best human name we have for the caller, or
// empty if none is known.
func (wa *WhatsAppClient) callerDisplayName(ctx context.Context, pn, lid types.JID) string {
	for _, jid := range []types.JID{pn, lid} {
		if jid.IsEmpty() {
			continue
		}
		contact, err := wa.GetStore().Contacts.GetContact(ctx, jid)
		if err != nil || !contact.Found {
			continue
		}
		for _, name := range []string{contact.FullName, contact.PushName, contact.BusinessName, contact.FirstName} {
			if name = strings.TrimSpace(name); name != "" {
				return name
			}
		}
	}
	return ""
}

type callReplyNotice struct {
	Data      callReplyTemplateData
	ReplyText string
	Sent      bool
}

func convertCallReplyNotice(_ context.Context, _ *bridgev2.Portal, _ bridgev2.MatrixAPI, n callReplyNotice) (*bridgev2.ConvertedMessage, error) {
	callWord := "call"
	if n.Data.CallType != "" {
		callWord = n.Data.CallType + " call"
	}
	who := n.Data.Name
	if who == "" {
		who = "unknown caller"
	}
	var body, formatted strings.Builder
	fmt.Fprintf(&body, "Declined incoming WhatsApp %s from %s.", callWord, who)
	fmt.Fprintf(&formatted, "Declined incoming WhatsApp %s from <b>%s</b>.", html.EscapeString(callWord), html.EscapeString(who))
	if n.Sent {
		fmt.Fprintf(&body, " Sent them: “%s”", n.ReplyText)
		fmt.Fprintf(&formatted, " Sent them: <i>“%s”</i>", html.EscapeString(n.ReplyText))
	} else {
		body.WriteString(" No text was sent (cooldown).")
		formatted.WriteString(" No text was sent (cooldown).")
	}
	if n.Data.CallLink != "" {
		fmt.Fprintf(&body, "\nJoin the call: %s", n.Data.CallLink)
		fmt.Fprintf(&formatted, "<br>Join the call: <a href=\"%[1]s\">%[1]s</a>", html.EscapeString(n.Data.CallLink))
	}
	return &bridgev2.ConvertedMessage{
		Parts: []*bridgev2.ConvertedMessagePart{{
			Type: event.EventMessage,
			Content: &event.MessageEventContent{
				MsgType:       event.MsgText,
				Body:          body.String(),
				Format:        event.FormatHTML,
				FormattedBody: formatted.String(),
			},
		}},
	}, nil
}
