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
	"fmt"
	"sort"
	"strings"
	"time"

	"go.mau.fi/whatsmeow/types"
	"maunium.net/go/mautrix/bridgev2/commands"
	"maunium.net/go/mautrix/bridgev2/networkid"

	"go.mau.fi/mautrix-whatsapp/pkg/waid"
)

// cmdFreeze and cmdUnfreeze manage the homestacks frozen chats (see freeze.go
// and HOMESTACKS.md). They run in the chat's own room, or anywhere with a
// phone number for that contact's DM.
var cmdFreeze = &commands.FullHandler{
	Func: fnFreeze,
	Name: "freeze",
	Help: commands.HelpMeta{
		Section:     commands.HelpSectionGeneral,
		Description: "Stop bridging new WhatsApp messages in this chat (or a number's DM) until it is unfrozen.",
		Args:        "[_phone number_|list]",
	},
	RequiresLogin: true,
}

var cmdUnfreeze = &commands.FullHandler{
	Func: fnUnfreeze,
	Name: "unfreeze",
	Help: commands.HelpMeta{
		Section:     commands.HelpSectionGeneral,
		Description: "Unfreeze this chat (or a number's DM) and bridge the messages that arrived meanwhile.",
		Args:        "[_phone number_]",
	},
	RequiresLogin: true,
}

const freezeUsage = "Usage: `$cmdprefix freeze [phone number|list]`, `$cmdprefix unfreeze [phone number]`\n\n" +
	"Without a number, run it in the chat's room. WhatsApp messages in a frozen chat are kept by the bridge, " +
	"not shown here; unfreezing brings them all in, in order. You can't send in a frozen chat."

// freezeTarget resolves the chat a freeze command is about: the number's DM
// (on your default WhatsApp account) if one is given, else the current room.
func freezeTarget(ce *commands.Event) (networkid.PortalKey, string, bool) {
	if len(ce.Args) == 0 {
		if ce.Portal == nil {
			ce.Reply(freezeUsage)
			return networkid.PortalKey{}, "", false
		}
		if jid, err := waid.ParsePortalID(ce.Portal.ID); err == nil && jid.Server == types.DefaultUserServer {
			// Incoming messages are keyed by LID, so freezing the phone-number
			// key would catch nothing. The bridge re-keys such a DM when its
			// next message arrives.
			ce.Reply("This chat still uses WhatsApp's old phone-number ID, so it can't be frozen yet. " +
				"It switches over by itself once a message arrives here; try again then.")
			return networkid.PortalKey{}, "", false
		}
		return ce.Portal.PortalKey, "This chat", true
	}
	number := strings.Join(ce.Args, "")
	login := ce.User.GetDefaultLogin()
	if login == nil {
		ce.Reply("You're not logged in to WhatsApp.")
		return networkid.PortalKey{}, "", false
	}
	wa, ok := login.Client.(*WhatsAppClient)
	if !ok {
		ce.Reply("Unexpected login type")
		return networkid.PortalKey{}, "", false
	}
	jid, err := wa.validateIdentifer(ce.Ctx, number)
	if err == nil {
		// DM portals are keyed by LID; a phone-number key would never match.
		jid, err = wa.startChatPNToLID(ce.Ctx, jid)
	}
	if err != nil {
		ce.Reply("Can't find %s on WhatsApp: %v", number, err)
		return networkid.PortalKey{}, "", false
	}
	return wa.makeWAPortalKey(jid), "Your chat with " + number, true
}

func freezeChatName(ce *commands.Event, key networkid.PortalKey) string {
	portal, err := ce.Bridge.GetExistingPortalByKey(ce.Ctx, key)
	if err != nil || portal == nil {
		return string(key.ID)
	} else if portal.Name != "" {
		return portal.Name
	} else if portal.OtherUserID != "" {
		if ghost, err := ce.Bridge.GetExistingGhostByID(ce.Ctx, portal.OtherUserID); err == nil && ghost != nil && ghost.Name != "" {
			return ghost.Name
		}
	}
	return string(key.ID)
}

func fnFreeze(ce *commands.Event) {
	if frozen.store == nil {
		ce.Reply("Freezing is not available on this bridge.")
		return
	}
	if len(ce.Args) == 1 && strings.EqualFold(ce.Args[0], "list") {
		fnFreezeList(ce)
		return
	}
	key, what, ok := freezeTarget(ce)
	if !ok {
		return
	}
	since, already, err := frozen.freeze(ce.Ctx, key, time.Now())
	if err != nil {
		ce.Reply("Failed to freeze: %v", err)
		return
	}
	loc := userLocation(ce.User.GetDefaultLogin())
	if already {
		n, _ := frozen.store.countEvents(ce.Ctx, key)
		ce.Reply("%s has been frozen since %s; %d message(s) waiting.", what, since.In(loc).Format("2006-01-02 15:04 MST"), n)
		return
	}
	ce.Reply("%s is frozen: new WhatsApp messages are kept back until `$cmdprefix unfreeze`, and you can't send here.", what)
}

func fnFreezeList(ce *commands.Event) {
	chats := frozen.list()
	if len(chats) == 0 {
		ce.Reply("No chats are frozen.")
		return
	}
	logins := make(map[networkid.UserLoginID]bool)
	for _, id := range ce.User.GetUserLoginIDs() {
		logins[id] = true
	}
	loc := userLocation(ce.User.GetDefaultLogin())
	var lines []string
	for key, since := range chats {
		// Only DMs carry a receiver; a shared group shows for everyone in it.
		if key.Receiver != "" && !logins[key.Receiver] {
			continue
		}
		n, _ := frozen.store.countEvents(ce.Ctx, key)
		lines = append(lines, fmt.Sprintf("* %s — since %s, %d waiting", freezeChatName(ce, key), since.In(loc).Format("2006-01-02 15:04 MST"), n))
	}
	if len(lines) == 0 {
		ce.Reply("No chats are frozen.")
		return
	}
	sort.Strings(lines)
	ce.Reply("Frozen chats:\n\n%s", strings.Join(lines, "\n"))
}

func fnUnfreeze(ce *commands.Event) {
	if frozen.store == nil {
		ce.Reply("Freezing is not available on this bridge.")
		return
	}
	key, what, ok := freezeTarget(ce)
	if !ok {
		return
	}
	if _, isFrozen := frozen.since(key); !isFrozen {
		ce.Reply("%s isn't frozen.", what)
		return
	}
	wc := ce.Bridge.Network.(*WhatsAppConnector)
	replayed, dropped, err := wc.unfreeze(ce.Ctx, key)
	if err != nil {
		ce.Reply("%s is still frozen: bridged %d held message(s), then stopped: %v. Run it again to retry.", what, replayed, err)
		return
	}
	msg := fmt.Sprintf("%s is unfrozen; %d held message(s) bridged.", what, replayed)
	if dropped > 0 {
		msg += fmt.Sprintf(" %d could not be read (or their account is gone) and were dropped.", dropped)
	}
	ce.Reply("%s", msg)
}
