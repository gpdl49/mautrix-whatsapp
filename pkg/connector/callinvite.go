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

	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"google.golang.org/protobuf/proto"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/commands"
	"maunium.net/go/mautrix/bridgev2/simplevent"
	"maunium.net/go/mautrix/event"

	"go.mau.fi/mautrix-whatsapp/pkg/waid"
)

// cmdCall is the outgoing half of the call feature: WhatsApp contacts cannot
// join a call in the (encrypted, bridged) portal room, so instead of starting
// one there the user runs `!wa call` and the bridge texts the chat a link to a
// fresh call room -- the same kind of room callroom.go makes for incoming
// calls. When the contact opens the link, Element Call rings the user.
var cmdCall = &commands.FullHandler{
	Func: fnCall,
	Name: "call",
	Help: commands.HelpMeta{
		Section:     commands.HelpSectionGeneral,
		Description: "Start a Matrix call and send this WhatsApp chat an invite link to it.",
	},
	RequiresPortal: true,
	RequiresLogin:  true,
}

var errCallInviteUnsupportedChat = errors.New("calls can only be started in WhatsApp direct and group chats")

// callInviteChatSupported reports whether an invite can be sent to a chat.
// Newsletters and broadcast lists have no one to call back.
func callInviteChatSupported(chat types.JID) bool {
	switch chat.Server {
	case types.DefaultUserServer, types.HiddenUserServer, types.GroupServer:
		return chat != types.StatusBroadcastJID
	default:
		return false
	}
}

func fnCall(ce *commands.Event) {
	login, _, err := ce.Portal.FindPreferredLogin(ce.Ctx, ce.User, false)
	if err != nil || login == nil {
		ce.Reply("You're not logged into WhatsApp in this chat.")
		return
	}
	wa, ok := login.Client.(*WhatsAppClient)
	if !ok || !wa.IsLoggedIn() {
		ce.Reply("That WhatsApp login is not connected.")
		return
	}
	if wa.Main.Config.CallAutoReply.CallLinkBaseURL == "" {
		ce.Reply("Call links are not configured on this bridge (`call_auto_reply.call_link_base_url`).")
		return
	}
	chat, err := waid.ParsePortalID(ce.Portal.ID)
	if err != nil {
		ce.Reply("Failed to parse this chat: %v", err)
		return
	}
	link, err := wa.sendCallInvite(ce.Ctx, ce.Portal, chat)
	if err != nil {
		ce.Reply("Failed to send the call invite: %v", err)
		return
	}
	ce.Reply("Sent a call invite. You'll be rung when they join; the call room is in your room list, or join now: %s", link)
}

// sendCallInvite creates a call room, texts its link to the chat from this
// login, and mirrors the text into the portal as the user's own message.
func (wa *WhatsAppClient) sendCallInvite(ctx context.Context, portal *bridgev2.Portal, chat types.JID) (string, error) {
	if !callInviteChatSupported(chat) {
		return "", errCallInviteUnsupportedChat
	}
	cfg := &wa.Main.Config.CallAutoReply
	data := callReplyTemplateData{
		Name:    portal.Name,
		Account: callReplyAccount(wa.UserLogin),
	}
	if chat.Server != types.GroupServer {
		pn, lid := chat, types.EmptyJID
		if chat.Server == types.HiddenUserServer {
			lid = chat
			pn, _ = wa.GetStore().LIDs.GetPNForLID(ctx, lid)
		}
		if name := wa.callerDisplayName(ctx, pn, lid); name != "" {
			data.Name = name
		}
		if !pn.IsEmpty() {
			data.Phone = "+" + pn.User
		}
	}

	roomName := "WhatsApp call"
	if data.Name != "" {
		roomName = "WhatsApp call with " + data.Name
	}
	roomID, link, err := wa.createCallRoom(ctx, roomName)
	if err != nil {
		return "", err
	}
	data.CallLink = link
	// The room is useless once the TTL passes, whether or not the text went out.
	wa.scheduleCallRoomCleanup(roomID, cfg.RoomTTL)

	text, err := renderCallReply(cfg.inviteTemplate, data)
	if err != nil {
		return "", err
	}
	resp, err := wa.Client.SendMessage(ctx, chat, &waE2E.Message{Conversation: proto.String(text)})
	if err != nil {
		return "", err
	}

	// whatsmeow does not echo our own sends back to us, so put the text in the
	// portal ourselves under its real WhatsApp message ID. Edits, receipts and
	// any later echo from another device then line up with it.
	res := wa.UserLogin.QueueRemoteEvent(&simplevent.Message[string]{
		EventMeta: simplevent.EventMeta{
			Type:      bridgev2.RemoteEventMessage,
			PortalKey: portal.PortalKey,
			Sender: bridgev2.EventSender{
				IsFromMe: true,
				Sender:   waid.MakeUserID(resp.Sender),
			},
			Timestamp:   resp.Timestamp,
			StreamOrder: resp.Timestamp.Unix(),
		},
		ID:                 waid.MakeMessageID(chat, resp.Sender, resp.ID),
		Data:               text,
		ConvertMessageFunc: convertCallInviteText,
	})
	if !res.Success {
		wa.UserLogin.Log.Warn().Str("action", "call invite").Msg("Failed to mirror the call invite into the portal")
	}
	return link, nil
}

func convertCallInviteText(_ context.Context, _ *bridgev2.Portal, _ bridgev2.MatrixAPI, text string) (*bridgev2.ConvertedMessage, error) {
	return &bridgev2.ConvertedMessage{
		Parts: []*bridgev2.ConvertedMessagePart{{
			Type:    event.EventMessage,
			Content: &event.MessageEventContent{MsgType: event.MsgText, Body: text},
		}},
	}, nil
}
