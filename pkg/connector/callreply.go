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

// callReplyTracker remembers which calls were already handled and, per caller,
// when they last received an auto-reply and which call room they were sent.
// That way CallOffer + CallOfferNotice for one call, or a caller retrying three
// times in a row, produce a single text and a single room. One instance is
// shared by all logins; entries are keyed by login ID and caller.
//
// The tracker itself is pure and in-memory; callstate.go loads it from and
// writes it through to the bridge database so cooldowns survive restarts.
type callReplyTracker struct {
	lock      sync.Mutex
	seenCalls map[string]time.Time
	callers   map[callerKey]callerEntry
	lastPrune time.Time
}

// callerKey identifies a caller as seen by one login. Caller is the phone
// number JID, or the LID when no phone number is known.
type callerKey struct {
	LoginID string
	Caller  string
}

// callerEntry is what the tracker knows about one caller.
type callerEntry struct {
	// LastReply is when the caller was last actually texted. Zero if they
	// never were (e.g. a room was created during a cooldown that predates the
	// tracker, or the bridge was restarted from an older version).
	LastReply time.Time
	// RoomID and Link are the call room the caller was most recently sent,
	// reused for further calls during the cooldown so that a caller retrying
	// does not get a fresh room each time. Empty without call links.
	RoomID id.RoomID
	Link   string
	// RoomExpiresAt is when the room is cleaned up. Zero means never.
	RoomExpiresAt time.Time
	// ExpiresAt is when the entry stops mattering: the cooldown has run out
	// and the room is gone. Entries are pruned after it.
	ExpiresAt time.Time
}

// callSeenRetention is how long a call ID is remembered for dedupe. Call
// events older than callEventMaxAge are ignored anyway, so this only needs to
// be comfortably longer than that.
const callSeenRetention = time.Hour

// callRoomReuseMargin is how much life a call room must have left to be handed
// to a caller again. A link to a room that is cleaned up seconds later is
// worse than a fresh one.
const callRoomReuseMargin = 2 * time.Minute

var callReplies = newCallReplyTracker()

func newCallReplyTracker() *callReplyTracker {
	return &callReplyTracker{
		seenCalls: make(map[string]time.Time),
		callers:   make(map[callerKey]callerEntry),
	}
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

// inCooldown reports whether the caller was texted less than cooldown ago.
// It does not change anything: the cooldown only starts once a text has
// actually gone out (see recordReply).
func (t *callReplyTracker) inCooldown(now time.Time, key callerKey, cooldown time.Duration) bool {
	if cooldown <= 0 {
		return false
	}
	t.lock.Lock()
	defer t.lock.Unlock()
	e, ok := t.callers[key]
	return ok && !e.LastReply.IsZero() && now.Sub(e.LastReply) < cooldown
}

// reusableRoom returns the caller's last call room if it is still around for
// long enough to be worth handing out again.
func (t *callReplyTracker) reusableRoom(now time.Time, key callerKey) (id.RoomID, string, bool) {
	t.lock.Lock()
	defer t.lock.Unlock()
	e, ok := t.callers[key]
	if !ok || e.RoomID == "" || e.Link == "" {
		return "", "", false
	}
	if !e.RoomExpiresAt.IsZero() && e.RoomExpiresAt.Sub(now) < callRoomReuseMargin {
		return "", "", false
	}
	return e.RoomID, e.Link, true
}

// recordReply starts the caller's cooldown, and remembers the room they were
// sent (if any). Call it only once the text has actually been sent. It
// returns the updated entry so that it can be persisted.
func (t *callReplyTracker) recordReply(now time.Time, key callerKey, cooldown time.Duration, room callRoomRef) callerEntry {
	t.lock.Lock()
	defer t.lock.Unlock()
	e := callerEntry{LastReply: now}
	e.setRoom(room)
	e.ExpiresAt = e.expiry(cooldown)
	t.callers[key] = e
	return e
}

// recordRoom remembers a room handed to a caller without texting them (a
// call during the cooldown whose previous room was already gone), leaving
// the cooldown as it was.
func (t *callReplyTracker) recordRoom(key callerKey, cooldown time.Duration, room callRoomRef) callerEntry {
	t.lock.Lock()
	defer t.lock.Unlock()
	e := t.callers[key]
	e.setRoom(room)
	e.ExpiresAt = e.expiry(cooldown)
	t.callers[key] = e
	return e
}

// load replaces the entry for key, e.g. from the database at startup.
func (t *callReplyTracker) load(key callerKey, e callerEntry) {
	t.lock.Lock()
	defer t.lock.Unlock()
	t.callers[key] = e
}

func (e *callerEntry) setRoom(room callRoomRef) {
	e.RoomID, e.Link, e.RoomExpiresAt = room.RoomID, room.Link, room.ExpiresAt
}

// expiry is the later of the end of the cooldown and the room's cleanup.
func (e *callerEntry) expiry(cooldown time.Duration) time.Time {
	exp := e.LastReply.Add(max(cooldown, 0))
	if e.RoomExpiresAt.After(exp) {
		exp = e.RoomExpiresAt
	}
	return exp
}

func (t *callReplyTracker) pruneLocked(now time.Time) {
	if now.Sub(t.lastPrune) < callSeenRetention/4 {
		return
	}
	t.lastPrune = now
	for key, ts := range t.seenCalls {
		if now.Sub(ts) > callSeenRetention {
			delete(t.seenCalls, key)
		}
	}
	for key, e := range t.callers {
		if !now.Before(e.ExpiresAt) {
			delete(t.callers, key)
		}
	}
}

// callAutoReplySettings resolves the effective settings for this login:
// per-login overrides from `!wa call-reply` win over the bridge config.
func (wa *WhatsAppClient) callAutoReplySettings() (enabled bool, message string) {
	cfg := &wa.Main.Config.CallAutoReply
	meta, _ := wa.UserLogin.Metadata.(*waid.UserLoginMetadata)
	enabled, _ = callReplyEnabled(cfg, meta)
	message = cfg.Message
	if meta != nil && meta.CallAutoReply != nil && meta.CallAutoReply.Message != "" {
		message = meta.CallAutoReply.Message
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
		Account:  callReplyAccount(wa.UserLogin),
		CallType: callType,
	}
	if !pn.IsEmpty() {
		data.Phone = "+" + pn.User
	}
	if data.Name == "" {
		data.Name = data.Phone
	}

	cfg := &wa.Main.Config.CallAutoReply
	key := callerKey{LoginID: string(wa.UserLogin.ID), Caller: cooldownKey}
	// Check the cooldown before anything else: a caller retrying during it
	// gets neither another text nor another room.
	cooling := callReplies.inCooldown(time.Now(), key, cfg.Cooldown)

	// call-link: find or create the room the caller will be sent to. Element
	// Call only ever joins a room that already exists, so the link is worthless
	// without this. Everything behind this seam lives in callroom.go and
	// callstate.go and is the part of the fork that is NOT upstreamable -- see
	// "Upstreaming" in HOMESTACKS.md.
	var room callRoomRef
	newRoom := false
	if cfg.CallLinkBaseURL != "" {
		if roomID, link, ok := callReplies.reusableRoom(time.Now(), key); cooling && ok {
			room = callRoomRef{RoomID: roomID, Link: link}
			log.Debug().Stringer("room_id", roomID).Msg("Caller is within the cooldown, reusing their call room")
		} else {
			roomName := "Incoming WhatsApp call"
			if data.Name != "" {
				roomName = "WhatsApp call from " + data.Name
			}
			var err error
			room, err = wa.createTrackedCallRoom(ctx, roomName, cooldownKey)
			if err != nil {
				// Send the text anyway: a caller being told "I can't take
				// WhatsApp calls" without a link is still better than silence.
				log.Err(err).Msg("Failed to create the call room; replying without a link")
			} else {
				newRoom = true
				if cooling {
					// Remember it so the caller's next retry reuses it.
					wa.Main.saveCaller(ctx, key, callReplies.recordRoom(key, cfg.Cooldown, room))
				}
			}
		}
		data.CallLink = room.Link
	}

	var replyText string
	status := callReplyCooldown
	if !cooling {
		status = callReplyFailed
		tmpl, err := parseCallReplyTemplate(message)
		if err != nil {
			log.Err(err).Msg("Invalid call auto-reply template, falling back to bridge config")
			tmpl = cfg.messageTemplate
		}
		replyText, err = renderCallReply(tmpl, data)
		if err != nil {
			log.Err(err).Msg("Failed to render call auto-reply")
		} else if _, err = wa.Client.SendMessage(ctx, dm, &waE2E.Message{Conversation: proto.String(replyText)}); err != nil {
			log.Err(err).Msg("Failed to send call auto-reply")
		} else {
			status = callReplySent
			log.Info().Msg("Sent call auto-reply")
			// Only a text that actually went out starts the cooldown.
			wa.Main.saveCaller(ctx, key, callReplies.recordReply(time.Now(), key, cfg.Cooldown, room))
		}
		if status != callReplySent && newRoom {
			// call-link: nobody was given the link, so the room is useless.
			wa.Main.cleanupCallRoom(ctx, room.RoomID, wa.UserLogin.UserMXID)
			data.CallLink = ""
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
		Data:               callReplyNotice{Data: data, ReplyText: replyText, Status: status},
		ConvertMessageFunc: convertCallReplyNotice,
	})
	if !res.Success {
		log.Warn().Msg("Failed to queue call auto-reply notice to Matrix")
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

// callReplyStatus says what happened to the text for one call.
type callReplyStatus int

const (
	callReplySent callReplyStatus = iota
	// The caller was already texted within the cooldown.
	callReplyCooldown
	// Rendering or sending the text failed; the cooldown was not started.
	callReplyFailed
)

type callReplyNotice struct {
	Data      callReplyTemplateData
	ReplyText string
	Status    callReplyStatus
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
	switch n.Status {
	case callReplySent:
		fmt.Fprintf(&body, " Sent them: “%s”", n.ReplyText)
		fmt.Fprintf(&formatted, " Sent them: <i>“%s”</i>", html.EscapeString(n.ReplyText))
	case callReplyCooldown:
		body.WriteString(" No text was sent: they were already texted recently (cooldown).")
		formatted.WriteString(" No text was sent: they were already texted recently (cooldown).")
	default:
		body.WriteString(" Texting them failed; see the bridge log.")
		formatted.WriteString(" Texting them failed; see the bridge log.")
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
