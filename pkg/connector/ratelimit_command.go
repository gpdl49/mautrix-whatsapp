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
	"strconv"
	"strings"
	"time"

	"go.mau.fi/util/jsontime"
	"maunium.net/go/mautrix/bridgev2/commands"

	"go.mau.fi/mautrix-whatsapp/pkg/waid"
)

// cmdRateLimit manages the homestacks per-chat send limit (see ratelimit.go
// and HOMESTACKS.md). It runs in the chat's own room.
var cmdRateLimit = &commands.FullHandler{
	Func: fnRateLimit,
	Name: "rate-limit",
	Help: commands.HelpMeta{
		Section:     commands.HelpSectionGeneral,
		Description: "Limit how many messages you can send in this chat relative to the other side.",
		Args:        "<show|off|reply [_burst_]|ratio <_mine_:_theirs_> [_burst_]>",
	},
	RequiresPortal: true,
	RequiresLogin:  true,
}

const rateLimitUsage = "Usage: `$cmdprefix rate-limit <show|off|reply [burst]|ratio <mine:theirs> [burst]>`\n\n" +
	"* `reply` — you can send up to `burst` (default 1) messages, then need a reply.\n" +
	"* `ratio 2:3` — you earn 2 messages for every 3 they send, holding at most `burst` (default the first number).\n\n" +
	"Messages over the limit are not sent to WhatsApp and the bridge tells you why. Setting a limit starts it fresh."

const rateLimitMaxValue = 100

// parseRateLimitArgs turns `reply [burst]` or `ratio M:N [burst]` into settings.
func parseRateLimitArgs(args []string, now time.Time) (*waid.RateLimitSettings, error) {
	if len(args) == 0 {
		return nil, errors.New("missing mode")
	}
	parseCount := func(what, s string) (int, error) {
		n, err := strconv.Atoi(s)
		if err != nil || n < 1 || n > rateLimitMaxValue {
			return 0, fmt.Errorf("%s must be a whole number from 1 to %d", what, rateLimitMaxValue)
		}
		return n, nil
	}
	s := &waid.RateLimitSettings{EnabledAt: jsontime.UM(now)}
	rest := args[1:]
	switch strings.ToLower(args[0]) {
	case waid.RateLimitModeReply:
		s.Mode, s.Burst = waid.RateLimitModeReply, 1
	case waid.RateLimitModeRatio:
		if len(rest) == 0 {
			return nil, errors.New("ratio needs a value like 2:3")
		}
		mine, theirs, ok := strings.Cut(rest[0], ":")
		if !ok {
			return nil, errors.New("ratio needs a value like 2:3")
		}
		var err error
		if s.Mine, err = parseCount("mine", mine); err != nil {
			return nil, err
		}
		if s.Theirs, err = parseCount("theirs", theirs); err != nil {
			return nil, err
		}
		s.Mode, s.Burst = waid.RateLimitModeRatio, s.Mine
		rest = rest[1:]
	default:
		return nil, fmt.Errorf("unknown mode %q", args[0])
	}
	if len(rest) > 1 {
		return nil, errors.New("too many arguments")
	}
	if len(rest) == 1 {
		burst, err := parseCount("burst", rest[0])
		if err != nil {
			return nil, err
		}
		s.Burst = burst
	}
	return s, nil
}

func fnRateLimit(ce *commands.Event) {
	meta, ok := ce.Portal.Metadata.(*waid.PortalMetadata)
	if !ok {
		ce.Reply("Unexpected portal metadata")
		return
	}
	sub := "show"
	if len(ce.Args) > 0 {
		sub = strings.ToLower(ce.Args[0])
	}
	switch sub {
	case "show":
		if meta.RateLimit == nil {
			ce.Reply("No rate limit in this chat.")
			return
		}
		status := ""
		login, _, err := ce.Portal.FindPreferredLogin(ce.Ctx, ce.User, false)
		var wa *WhatsAppClient
		if err == nil && login != nil {
			wa, _ = login.Client.(*WhatsAppClient)
		}
		if wa != nil {
			if st, err := wa.readRateLimitState(ce.Ctx, ce.Portal, meta.RateLimit); err == nil {
				if st.Available > 0 {
					status = fmt.Sprintf(" You can send %d more now.", st.Available)
				} else {
					status = " Blocked: " + rateLimitRefusal(meta.RateLimit, st)
				}
			}
		}
		ce.Reply("Rate limit here: %s, since %s.%s", describeRateLimit(meta.RateLimit),
			meta.RateLimit.EnabledAt.Time.Format("2006-01-02 15:04 MST"), status)
	case "off":
		meta.RateLimit = nil
		if err := ce.Portal.Save(ce.Ctx); err != nil {
			ce.Reply("Failed to save: %v", err)
			return
		}
		ce.Reply("Rate limit removed for this chat.")
	case waid.RateLimitModeReply, waid.RateLimitModeRatio:
		s, err := parseRateLimitArgs(ce.Args, time.Now())
		if err != nil {
			ce.Reply("%v\n\n%s", err, rateLimitUsage)
			return
		}
		meta.RateLimit = s
		if err := ce.Portal.Save(ce.Ctx); err != nil {
			ce.Reply("Failed to save: %v", err)
			return
		}
		ce.Reply("Rate limit set for this chat: %s.", describeRateLimit(s))
	default:
		ce.Reply(rateLimitUsage)
	}
}
