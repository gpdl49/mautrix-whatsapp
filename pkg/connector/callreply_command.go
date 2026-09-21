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
	"html"
	"strings"

	"go.mau.fi/util/ptr"
	"maunium.net/go/mautrix/bridgev2/commands"

	"go.mau.fi/mautrix-whatsapp/pkg/waid"
)

// cmdCallReply lets a user manage the homestacks call auto-reply for their
// login (see callreply.go and HOMESTACKS.md).
var cmdCallReply = &commands.FullHandler{
	Func: fnCallReply,
	Name: "call-reply",
	Help: commands.HelpMeta{
		Section:     commands.HelpSectionGeneral,
		Description: "Manage the automatic text sent to people who call you on WhatsApp.",
		Args:        "<show|set <message>|clear|on|off>",
	},
	RequiresLogin: true,
}

const callReplyUsage = "Usage: `$cmdprefix call-reply <show|set <message>|clear|on|off>`\n\n" +
	"Placeholders for `set`: `{{.Name}}` (caller's name), `{{.Phone}}`, `{{.CallLink}}` (Element Call link), `{{.CallType}}`."

func fnCallReply(ce *commands.Event) {
	login := ce.User.GetDefaultLogin()
	if login == nil {
		ce.Reply("Login not found")
		return
	}
	wa, ok := login.Client.(*WhatsAppClient)
	if !ok {
		ce.Reply("Login is not connected")
		return
	}
	meta, ok := login.Metadata.(*waid.UserLoginMetadata)
	if !ok {
		ce.Reply("Unexpected login metadata")
		return
	}
	if len(ce.Args) == 0 {
		ce.Reply(callReplyUsage)
		return
	}
	sub := strings.ToLower(ce.Args[0])
	switch sub {
	case "show":
		enabled, message := wa.callAutoReplySettings()
		state := "disabled"
		if enabled {
			state = "enabled"
		}
		source := "bridge default"
		if meta.CallAutoReply != nil && meta.CallAutoReply.Message != "" {
			source = "set by you"
		}
		link := wa.Main.Config.CallAutoReply.CallLinkBaseURL
		if link == "" {
			link = "(no call link configured)"
		}
		ce.ReplyAdvanced(fmt.Sprintf(
			"Call auto-reply is <b>%s</b>. Message (%s):<br><i>%s</i><br>Call link base: <code>%s</code>",
			state, source, html.EscapeString(message), html.EscapeString(link),
		), false, true)
	case "set":
		text := strings.TrimSpace(strings.TrimPrefix(ce.RawArgs, ce.Args[0]))
		if text == "" {
			ce.Reply(callReplyUsage)
			return
		}
		if _, err := parseCallReplyTemplate(text); err != nil {
			ce.Reply("Invalid message template: %v", err)
			return
		}
		if meta.CallAutoReply == nil {
			meta.CallAutoReply = &waid.CallAutoReplySettings{}
		}
		meta.CallAutoReply.Message = text
		if err := login.Save(ce.Ctx); err != nil {
			ce.Reply("Failed to save settings: %v", err)
			return
		}
		ce.React("✅")
	case "clear":
		if meta.CallAutoReply != nil {
			meta.CallAutoReply.Message = ""
			if meta.CallAutoReply.Enabled == nil {
				meta.CallAutoReply = nil
			}
		}
		if err := login.Save(ce.Ctx); err != nil {
			ce.Reply("Failed to save settings: %v", err)
			return
		}
		ce.Reply("Reset the call auto-reply message to the bridge default.")
	case "on", "off":
		if meta.CallAutoReply == nil {
			meta.CallAutoReply = &waid.CallAutoReplySettings{}
		}
		meta.CallAutoReply.Enabled = ptr.Ptr(sub == "on")
		if err := login.Save(ce.Ctx); err != nil {
			ce.Reply("Failed to save settings: %v", err)
			return
		}
		ce.Reply("Call auto-reply is now %s for your login.", sub)
	default:
		ce.Reply(callReplyUsage)
	}
}
