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
	"net/url"
	"strings"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/types"
	"maunium.net/go/mautrix/bridgev2/networkid"

	"go.mau.fi/mautrix-whatsapp/pkg/waid"
)

func TestCallAutoReplyConfigPostProcess(t *testing.T) {
	cfg := CallAutoReplyConfig{
		CallLinkBaseURL:    " https://call.example.org/ ",
		GuestHomeserverURL: " https://guests.example.org/ ",
	}
	if err := cfg.postProcess(); err != nil {
		t.Fatalf("postProcess: %v", err)
	}
	if cfg.Message != defaultCallAutoReplyMessage {
		t.Errorf("empty message should fall back to default, got %q", cfg.Message)
	}
	if cfg.CallLinkBaseURL != "https://call.example.org" {
		t.Errorf("base URL should be trimmed, got %q", cfg.CallLinkBaseURL)
	}
	if cfg.GuestHomeserverURL != "https://guests.example.org" {
		t.Errorf("guest homeserver URL should be trimmed, got %q", cfg.GuestHomeserverURL)
	}
	if cfg.messageTemplate == nil {
		t.Error("template should be parsed")
	}

	// A link with no guest homeserver loads fine and then fails at the point
	// the call starts, which is the worst possible time to find out. Startup
	// must refuse it rather than hand callers a link that cannot work.
	noGuest := CallAutoReplyConfig{CallLinkBaseURL: "https://call.example.org"}
	if err := noGuest.postProcess(); err == nil {
		t.Error("call_link_base_url without guest_homeserver_url should be rejected at startup")
	}
	// Neither set is a valid configuration: no link is generated at all.
	if err := (&CallAutoReplyConfig{}).postProcess(); err != nil {
		t.Errorf("no link configured at all should be fine: %v", err)
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

func TestBuildCallLink(t *testing.T) {
	cfg := CallAutoReplyConfig{
		CallLinkBaseURL:    "https://call.example.org",
		GuestHomeserverURL: "https://guests.example.org",
		ViaServers:         []string{"example.org"},
	}
	link := cfg.buildCallLink("!abc:example.org")

	base, query, found := strings.Cut(link, "?")
	if !found {
		t.Fatalf("link has no query: %q", link)
	}
	// Element Call reads everything from the fragment; a link that puts the
	// room in the path is the deprecated form and creates nothing.
	if base != "https://call.example.org/room/#/callback" {
		t.Errorf("unexpected link base %q", base)
	}
	q, err := url.ParseQuery(query)
	if err != nil {
		t.Fatalf("parse query: %v", err)
	}
	if got := q.Get("roomId"); got != "!abc:example.org" {
		t.Errorf("roomId = %q", got)
	}
	if got := q.Get("homeserver"); got != "https://guests.example.org" {
		t.Errorf("homeserver = %q -- without this the caller registers somewhere that cannot reach the room", got)
	}
	if got := q.Get("viaServers"); got != "example.org" {
		t.Errorf("viaServers = %q", got)
	}
	if got := q.Get("sendNotificationType"); got != "ring" {
		t.Errorf("sendNotificationType = %q -- this is what rings the user", got)
	}
	// The password is the call's media encryption key and the link is the only
	// credential a caller has, so it must be present and not trivially short.
	if pw := q.Get("password"); len(pw) < 16 {
		t.Errorf("password = %q, want something unguessable", pw)
	}

	// Two links for the same room must not share a media key.
	if cfg.buildCallLink("!abc:example.org") == link {
		t.Error("two links for the same room produced an identical password")
	}
}

func TestCallRoomMemberEventsArePermitted(t *testing.T) {
	// Both names on purpose: current clients write the legacy
	// org.matrix.msc3401.call.member, and granting only m.rtc.member looks
	// correct while leaving the call unable to start.
	want := map[string]bool{"m.rtc.member": false, "org.matrix.msc3401.call.member": false}
	for _, evt := range callRoomMemberEvents {
		if _, ok := want[evt]; !ok {
			t.Errorf("unexpected event type %q", evt)
			continue
		}
		want[evt] = true
	}
	for evt, seen := range want {
		if !seen {
			t.Errorf("%s must be granted to members or the call cannot start", evt)
		}
	}
}

func TestCallReplyEnabledPerLogin(t *testing.T) {
	for _, bridgeDefault := range []bool{false, true} {
		cfg := &CallAutoReplyConfig{Enabled: bridgeDefault}
		if got, src := callReplyEnabled(cfg, nil); got != bridgeDefault || src != "bridge default" {
			t.Errorf("no metadata: got (%v, %q), want bridge default %v", got, src, bridgeDefault)
		}
		if got, _ := callReplyEnabled(cfg, &waid.UserLoginMetadata{CallAutoReply: &waid.CallAutoReplySettings{Message: "hi"}}); got != bridgeDefault {
			t.Errorf("message-only override changed enabled to %v", got)
		}
		for _, override := range []bool{false, true} {
			meta := &waid.UserLoginMetadata{CallAutoReply: &waid.CallAutoReplySettings{Enabled: &override}}
			if got, src := callReplyEnabled(cfg, meta); got != override || src != "set for this login" {
				t.Errorf("default %v, login %v: got (%v, %q)", bridgeDefault, override, got, src)
			}
		}
	}
}

func TestCallInviteChatSupported(t *testing.T) {
	tests := map[types.JID]bool{
		types.NewJID("15550000000", types.DefaultUserServer):       true,
		types.NewJID("123456789", types.HiddenUserServer):          true,
		types.NewJID("120363000000000000", types.GroupServer):      true,
		types.StatusBroadcastJID:                                   false,
		types.NewJID("120363000000000000", types.NewsletterServer): false,
	}
	for jid, want := range tests {
		if got := callInviteChatSupported(jid); got != want {
			t.Errorf("%s: got %v, want %v", jid, got, want)
		}
	}
}

func TestDefaultCallInviteMessageParses(t *testing.T) {
	cfg := CallAutoReplyConfig{}
	if err := cfg.postProcess(); err != nil {
		t.Fatal(err)
	}
	text, err := renderCallReply(cfg.inviteTemplate, callReplyTemplateData{CallLink: "https://call.example.org/abc"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, "https://call.example.org/abc") {
		t.Fatalf("invite text %q does not contain the link", text)
	}
}

func TestSelectCallReplyLogin(t *testing.T) {
	two := []networkid.UserLoginID{"15550000001", "15550000002"}
	tests := []struct {
		name         string
		owned        []networkid.UserLoginID
		firstArg     string
		receiver     networkid.UserLoginID
		wantID       networkid.UserLoginID
		wantConsumed bool
		wantErr      bool
	}{
		{name: "only login", owned: two[:1], firstArg: "show", wantID: "15550000001"},
		{name: "named login", owned: two, firstArg: "15550000002", wantID: "15550000002", wantConsumed: true},
		{name: "named login with plus", owned: two, firstArg: "+15550000002", wantID: "15550000002", wantConsumed: true},
		{name: "named login beats portal", owned: two, firstArg: "15550000002", receiver: "15550000001", wantID: "15550000002", wantConsumed: true},
		{name: "portal receiver", owned: two, firstArg: "show", receiver: "15550000002", wantID: "15550000002"},
		{name: "someone else's portal", owned: two, firstArg: "show", receiver: "15559999999", wantErr: true},
		{name: "unknown login ID", owned: two, firstArg: "15559999999", wantErr: true},
		{name: "ambiguous", owned: two, firstArg: "show", wantErr: true},
		{name: "no logins", firstArg: "show", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			id, consumed, err := selectCallReplyLogin(tt.owned, tt.firstArg, tt.receiver)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if id != tt.wantID || consumed != tt.wantConsumed {
				t.Fatalf("got (%q, %v), want (%q, %v)", id, consumed, tt.wantID, tt.wantConsumed)
			}
		})
	}
}
