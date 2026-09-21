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
	"strings"
	"testing"
	"time"
)

func TestCallAutoReplyConfigPostProcess(t *testing.T) {
	cfg := CallAutoReplyConfig{CallLinkBaseURL: " https://call.example.org/ "}
	if err := cfg.postProcess(); err != nil {
		t.Fatalf("postProcess: %v", err)
	}
	if cfg.Message != defaultCallAutoReplyMessage {
		t.Errorf("empty message should fall back to default, got %q", cfg.Message)
	}
	if cfg.CallLinkBaseURL != "https://call.example.org" {
		t.Errorf("base URL should be trimmed, got %q", cfg.CallLinkBaseURL)
	}
	if cfg.messageTemplate == nil {
		t.Error("template should be parsed")
	}

	bad := CallAutoReplyConfig{Message: "hello {{.Nope}}"}
	if err := bad.postProcess(); err == nil {
		t.Error("unknown placeholder should be rejected at startup")
	}
	bad = CallAutoReplyConfig{Message: "hello {{.Name"}
	if err := bad.postProcess(); err == nil {
		t.Error("syntax error should be rejected at startup")
	}
}

func TestRenderCallReply(t *testing.T) {
	tmpl, err := parseCallReplyTemplate("Hi {{.Name}} ({{.Phone}}), {{.CallType}} call → {{.CallLink}}  ")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	out, err := renderCallReply(tmpl, callReplyTemplateData{
		Name: "Ada", Phone: "+15550000000", CallLink: "https://call.example.org/abc", CallType: "video",
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	want := "Hi Ada (+15550000000), video call → https://call.example.org/abc"
	if out != want {
		t.Errorf("got %q, want %q", out, want)
	}
}

func TestCallReplyTrackerDedupesCalls(t *testing.T) {
	tr := &callReplyTracker{seenCalls: map[string]time.Time{}, lastReply: map[string]time.Time{}}
	now := time.Now()
	if !tr.markCall(now, "login", "call-1") {
		t.Error("first sighting of a call should be handled")
	}
	if tr.markCall(now.Add(time.Second), "login", "call-1") {
		t.Error("second sighting of the same call (e.g. CallOfferNotice after CallOffer) must be ignored")
	}
	if !tr.markCall(now, "other-login", "call-1") {
		t.Error("call IDs are scoped per login")
	}
	if !tr.markCall(now.Add(2*callReplyTrackerRetention), "login", "call-1") {
		t.Error("entries should be pruned after the retention period")
	}
}

func TestCallReplyTrackerCooldown(t *testing.T) {
	tr := &callReplyTracker{seenCalls: map[string]time.Time{}, lastReply: map[string]time.Time{}}
	now := time.Now()
	const cooldown = 10 * time.Minute
	if !tr.markReply(now, "login", "caller", cooldown) {
		t.Error("first reply should be sent")
	}
	if tr.markReply(now.Add(time.Minute), "login", "caller", cooldown) {
		t.Error("retry within the cooldown must not get another text")
	}
	if !tr.markReply(now.Add(time.Minute), "login", "someone-else", cooldown) {
		t.Error("cooldown is per caller")
	}
	if !tr.markReply(now.Add(cooldown+time.Second), "login", "caller", cooldown) {
		t.Error("reply should be sent again after the cooldown")
	}
	if !tr.markReply(now, "login", "caller", 0) {
		t.Error("zero cooldown disables rate limiting")
	}
}

func TestConvertCallReplyNotice(t *testing.T) {
	msg, err := convertCallReplyNotice(context.Background(), nil, nil, callReplyNotice{
		Data:      callReplyTemplateData{Name: "Ada <3", CallType: "audio", CallLink: "https://call.example.org/abc"},
		ReplyText: "see you there",
		Sent:      true,
	})
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	content := msg.Parts[0].Content
	for _, want := range []string{"audio call", "Ada <3", "see you there", "https://call.example.org/abc"} {
		if !strings.Contains(content.Body, want) {
			t.Errorf("body missing %q: %q", want, content.Body)
		}
	}
	if !strings.Contains(content.FormattedBody, "Ada &lt;3") {
		t.Errorf("name should be HTML-escaped in formatted body: %q", content.FormattedBody)
	}
	if !strings.Contains(content.FormattedBody, `<a href="https://call.example.org/abc">`) {
		t.Errorf("link should be clickable: %q", content.FormattedBody)
	}

	msg, _ = convertCallReplyNotice(context.Background(), nil, nil, callReplyNotice{Data: callReplyTemplateData{}})
	body := msg.Parts[0].Content.Body
	if !strings.Contains(body, "unknown caller") || !strings.Contains(body, "cooldown") {
		t.Errorf("unexpected fallback body: %q", body)
	}
}
