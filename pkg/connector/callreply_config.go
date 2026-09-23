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
	"strings"
	"text/template"
	"time"

	up "go.mau.fi/util/configupgrade"
)

// CallAutoReplyConfig is the bridge-wide `call_auto_reply` config section for
// the homestacks call auto-reply feature (see HOMESTACKS.md). Logins can
// override Enabled and Message through `!wa call-reply`.
type CallAutoReplyConfig struct {
	Enabled           bool          `yaml:"enabled"`
	Message           string        `yaml:"message"`
	CallLinkBaseURL   string        `yaml:"call_link_base_url"`
	Cooldown          time.Duration `yaml:"cooldown"`
	IncludeGroupCalls bool          `yaml:"include_group_calls"`

	// call-link: the settings below drive the per-call Matrix room in
	// callroom.go. They are the non-upstreamable half of this feature; see
	// "Upstreaming" in HOMESTACKS.md before moving any of it.
	//
	// GuestHomeserverURL is the homeserver Element Call registers the caller
	// on. It must federate with the homeserver the call room lives on. Without
	// it Element Call uses its own default guest server, which federates with
	// nothing, so the caller can never reach the room -- hence the hard failure
	// in postProcess rather than a link that looks fine and cannot work.
	GuestHomeserverURL string `yaml:"guest_homeserver_url"`
	// ViaServers are the servers Element Call should try when joining the room
	// over federation. Normally just the server the call room is created on.
	ViaServers []string `yaml:"via_servers"`
	// RoomTTL is how long a call room is kept before the bridge and the user
	// leave it. Zero keeps rooms forever.
	RoomTTL time.Duration `yaml:"room_ttl"`

	messageTemplate *template.Template `yaml:"-"`
}

// callReplyTemplateData is what the auto-reply message template is rendered with.
type callReplyTemplateData struct {
	// Name is the caller's contact name, push name or business name, falling back to Phone.
	Name string
	// Phone is the caller's phone number in international format, or empty if only a LID is known.
	Phone string
	// Account is the user's own number that was called, in international format.
	// It tells callers apart when one Matrix user has several WhatsApp logins.
	Account string
	// CallLink is the link to this call's Matrix room, or empty if call links
	// are disabled or the room could not be created. call-link: see callroom.go.
	CallLink string
	// CallType is "audio" or "video" when WhatsApp told us, otherwise empty.
	CallType string
}

const defaultCallAutoReplyMessage = "Hi {{.Name}}, I can't receive WhatsApp calls on this number. You can call me right now here instead: {{.CallLink}}"

func upgradeCallAutoReplyConfig(helper up.Helper) {
	helper.Copy(up.Bool, "call_auto_reply", "enabled")
	helper.Copy(up.Str, "call_auto_reply", "message")
	helper.Copy(up.Str|up.Null, "call_auto_reply", "call_link_base_url")
	helper.Copy(up.Str|up.Null, "call_auto_reply", "guest_homeserver_url")
	helper.Copy(up.List|up.Null, "call_auto_reply", "via_servers")
	helper.Copy(up.Str|up.Int, "call_auto_reply", "cooldown")
	helper.Copy(up.Str|up.Int|up.Null, "call_auto_reply", "room_ttl")
	helper.Copy(up.Bool, "call_auto_reply", "include_group_calls")
}

// postProcess parses and validates the message template. It is called from
// Config.PostProcess so a bad template fails at startup rather than on the
// first call.
func (c *CallAutoReplyConfig) postProcess() error {
	if c.Message == "" {
		c.Message = defaultCallAutoReplyMessage
	}
	var err error
	c.messageTemplate, err = parseCallReplyTemplate(c.Message)
	if err != nil {
		return fmt.Errorf("failed to parse call_auto_reply.message template: %w", err)
	}
	c.CallLinkBaseURL = strings.TrimSuffix(strings.TrimSpace(c.CallLinkBaseURL), "/")
	c.GuestHomeserverURL = strings.TrimSuffix(strings.TrimSpace(c.GuestHomeserverURL), "/")
	// A base URL with no guest homeserver produces a link that loads, registers
	// the caller somewhere that cannot reach the room, and fails at the point
	// the call starts. That is far worse than refusing to start, so refuse.
	if c.CallLinkBaseURL != "" && c.GuestHomeserverURL == "" {
		return fmt.Errorf("call_auto_reply.guest_homeserver_url is required when call_link_base_url is set: " +
			"without it Element Call registers callers on a homeserver that cannot reach the call room")
	}
	return nil
}

// parseCallReplyTemplate parses a message template and executes it once with
// sample data so that references to unknown fields are caught early.
func parseCallReplyTemplate(text string) (*template.Template, error) {
	tmpl, err := template.New("call_auto_reply").Option("missingkey=error").Parse(text)
	if err != nil {
		return nil, err
	}
	_, err = renderCallReply(tmpl, callReplyTemplateData{
		Name:     "Example",
		Phone:    "+15550000000",
		Account:  "+15550000001",
		CallLink: "https://example.com/call",
		CallType: "audio",
	})
	if err != nil {
		return nil, err
	}
	return tmpl, nil
}

func renderCallReply(tmpl *template.Template, data callReplyTemplateData) (string, error) {
	var buf strings.Builder
	if err := tmpl.Execute(&buf, data); err != nil {
		return "", err
	}
	return strings.TrimSpace(buf.String()), nil
}
