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

	messageTemplate *template.Template `yaml:"-"`
}

// callReplyTemplateData is what the auto-reply message template is rendered with.
type callReplyTemplateData struct {
	// Name is the caller's contact name, push name or business name, falling back to Phone.
	Name string
	// Phone is the caller's phone number in international format, or empty if only a LID is known.
	Phone string
	// CallLink is the generated Element Call link, or empty if call_link_base_url is unset.
	CallLink string
	// CallType is "audio" or "video" when WhatsApp told us, otherwise empty.
	CallType string
}

const defaultCallAutoReplyMessage = "Hi {{.Name}}, I can't receive WhatsApp calls on this number. You can call me right now here instead: {{.CallLink}}"

func upgradeCallAutoReplyConfig(helper up.Helper) {
	helper.Copy(up.Bool, "call_auto_reply", "enabled")
	helper.Copy(up.Str, "call_auto_reply", "message")
	helper.Copy(up.Str|up.Null, "call_auto_reply", "call_link_base_url")
	helper.Copy(up.Str|up.Int, "call_auto_reply", "cooldown")
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
