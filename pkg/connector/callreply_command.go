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
	"errors"
	"fmt"
	"html"
	"slices"
	"strings"

	"go.mau.fi/util/ptr"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/commands"
	"maunium.net/go/mautrix/bridgev2/networkid"

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
		Args:        "[_login ID_] <show|set <message>|clear|on|off|reset>",
	},
	RequiresLogin: true,
}

const callReplyUsage = "Usage: `$cmdprefix call-reply [login ID] <show|set <message>|clear|on|off|reset>`\n\n" +
	"`on`/`off` apply to one login only and override the bridge default; `clear` resets the message and `reset` resets both. " +
	"With several WhatsApp logins, name the login (its phone number) or run the command in one of that login's chats.\n\n" +
	"Placeholders for `set`: `{{.Name}}` (caller's name), `{{.Phone}}`, `{{.Account}}` (your number that was called), " +
	"`{{.CallLink}}` (Element Call link), `{{.CallType}}`."

var errCallReplyLoginAmbiguous = errors.New("ambiguous login")

// selectCallReplyLogin picks the login a call-reply command applies to, in
// order: a leading argument naming one of the user's logins (with or without
// "+"), the receiver of the portal the command was sent in, or the user's only
// login. consumedArg reports whether the first argument was the login ID.
func selectCallReplyLogin(owned []networkid.UserLoginID, firstArg string, portalReceiver networkid.UserLoginID) (loginID networkid.UserLoginID, consumedArg bool, err error) {
	if candidate := networkid.UserLoginID(strings.TrimPrefix(firstArg, "+")); candidate != "" && slices.Contains(owned, candidate) {
		return candidate, true, nil
	}
	if portalReceiver != "" && slices.Contains(owned, portalReceiver) {
		return portalReceiver, false, nil
	}
	if len(owned) == 1 {
		return owned[0], false, nil
	}
	return "", false, errCallReplyLoginAmbiguous
}

func fnCallReply(ce *commands.Event) {
	var firstArg string
	if len(ce.Args) > 0 {
		firstArg = ce.Args[0]
	}
	var portalReceiver networkid.UserLoginID
	if ce.Portal != nil {
		portalReceiver = ce.Portal.Receiver
	}
	loginID, consumedArg, err := selectCallReplyLogin(ce.User.GetUserLoginIDs(), firstArg, portalReceiver)
	if err != nil {
		ce.Reply("You have several WhatsApp logins; name the one to change, e.g. `$cmdprefix call-reply <login ID> off`.\n\n%s", callReplyStatusList(ce))
		return
	}
	if consumedArg {
		ce.Args = ce.Args[1:]
		ce.RawArgs = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(ce.RawArgs), firstArg))
	}
	login := ce.Bridge.GetCachedUserLoginByID(loginID)
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
		_, enabledSource := callReplyEnabled(&wa.Main.Config.CallAutoReply, meta)
		source := "bridge default"
		if meta.CallAutoReply != nil && meta.CallAutoReply.Message != "" {
			source = "set for this login"
		}
		link := wa.Main.Config.CallAutoReply.CallLinkBaseURL
		if link == "" {
			link = "(no call link configured)"
		}
		ce.ReplyAdvanced(fmt.Sprintf(
			"Call auto-reply for <code>%s</code> is <b>%s</b> (%s). Message (%s):<br><i>%s</i><br>Call link base: <code>%s</code>",
			html.EscapeString(callReplyAccount(login)), state, enabledSource, source, html.EscapeString(message), html.EscapeString(link),
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
		ce.Reply("Reset the call auto-reply message for %s to the bridge default.", callReplyAccount(login))
	case "on", "off":
		if meta.CallAutoReply == nil {
			meta.CallAutoReply = &waid.CallAutoReplySettings{}
		}
		meta.CallAutoReply.Enabled = ptr.Ptr(sub == "on")
		if err := login.Save(ce.Ctx); err != nil {
			ce.Reply("Failed to save settings: %v", err)
			return
		}
		ce.Reply("Call auto-reply is now %s for %s.", sub, callReplyAccount(login))
	case "reset":
		meta.CallAutoReply = nil
		if err := login.Save(ce.Ctx); err != nil {
			ce.Reply("Failed to save settings: %v", err)
			return
		}
		ce.Reply("Call auto-reply for %s now follows the bridge default (%s).", callReplyAccount(login), onOff(wa.Main.Config.CallAutoReply.Enabled))
	default:
		ce.Reply(callReplyUsage)
	}
}

// callReplyAccount formats a login's own phone number the way callers see it.
func callReplyAccount(login *bridgev2.UserLogin) string {
	return "+" + waid.ParseUserLoginID(login.ID, 0).User
}

// callReplyEnabled resolves whether the auto-reply is on for a login and
// describes where that came from: the login's own on/off, or the bridge default.
func callReplyEnabled(cfg *CallAutoReplyConfig, meta *waid.UserLoginMetadata) (enabled bool, source string) {
	if meta != nil && meta.CallAutoReply != nil && meta.CallAutoReply.Enabled != nil {
		return *meta.CallAutoReply.Enabled, "set for this login"
	}
	return cfg.Enabled, "bridge default"
}

func onOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

// callReplyStatusList lists every login of the command's user with its
// effective on/off state, so picking one to change needs no second command.
func callReplyStatusList(ce *commands.Event) string {
	cfg := &ce.Bridge.Network.(*WhatsAppConnector).Config.CallAutoReply
	var buf strings.Builder
	buf.WriteString("Your logins:\n\n")
	ids := ce.User.GetUserLoginIDs()
	slices.Sort(ids)
	for _, loginID := range ids {
		login := ce.Bridge.GetCachedUserLoginByID(loginID)
		if login == nil {
			continue
		}
		meta, _ := login.Metadata.(*waid.UserLoginMetadata)
		enabled, source := callReplyEnabled(cfg, meta)
		fmt.Fprintf(&buf, "* `%s` (%s): auto-reply %s, %s\n", loginID, login.RemoteName, onOff(enabled), source)
	}
	return buf.String()
}
