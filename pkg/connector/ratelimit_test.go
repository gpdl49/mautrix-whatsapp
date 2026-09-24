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
	"testing"
	"time"

	"go.mau.fi/mautrix-whatsapp/pkg/waid"
)

// history turns "MMT" (M = mine, T = theirs), oldest first, into replay input.
func history(s string) []bool {
	out := make([]bool, len(s))
	for i, c := range s {
		out[i] = c == 'M'
	}
	return out
}

func TestReplayRateLimitReply(t *testing.T) {
	one := &waid.RateLimitSettings{Mode: waid.RateLimitModeReply, Burst: 1}
	three := &waid.RateLimitSettings{Mode: waid.RateLimitModeReply, Burst: 3}
	cases := []struct {
		name       string
		s          *waid.RateLimitSettings
		hist       string
		available  int
		unanswered int
	}{
		{"fresh", one, "", 1, 0},
		{"one unanswered", one, "M", 0, 1},
		{"answered", one, "MT", 1, 0},
		{"several replies don't stack", one, "TTTT", 1, 0},
		{"sent from phone past limit", one, "MM", 0, 2},
		{"burst 3 partially used", three, "TMM", 1, 2},
		{"burst 3 used up", three, "MMM", 0, 3},
		{"burst 3 refilled by one reply", three, "MMMT", 3, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			st := replayRateLimit(c.s, history(c.hist))
			if st.Available != c.available || st.Unanswered != c.unanswered {
				t.Fatalf("got %+v, want available=%d unanswered=%d", st, c.available, c.unanswered)
			}
			if (st.Available == 0) != (st.RepliesNeeded == 1) {
				t.Fatalf("reply mode should need exactly one reply when blocked, got %+v", st)
			}
		})
	}
}

func TestReplayRateLimitRatio(t *testing.T) {
	s := &waid.RateLimitSettings{Mode: waid.RateLimitModeRatio, Mine: 2, Theirs: 3, Burst: 2}
	cases := []struct {
		hist          string
		available     int
		repliesNeeded int
	}{
		{"", 2, 0},
		{"MM", 0, 2},
		{"MMT", 0, 1},
		{"MMTT", 1, 0},
		{"MMTTT", 2, 0},
		{"MMTTTM", 1, 0},
		// Credit is capped at burst: a long quiet stretch banks nothing extra.
		{"TTTTTTTTT", 2, 0},
		// Phone-sent messages can overdraw; recovering takes proportionally more.
		{"MMMM", 0, 5},
	}
	for _, c := range cases {
		t.Run(c.hist, func(t *testing.T) {
			st := replayRateLimit(s, history(c.hist))
			if st.Available != c.available || st.RepliesNeeded != c.repliesNeeded {
				t.Fatalf("got %+v, want available=%d repliesNeeded=%d", st, c.available, c.repliesNeeded)
			}
		})
	}
}

func TestParseRateLimitArgs(t *testing.T) {
	now := time.UnixMilli(1_700_000_000_000)
	good := []struct {
		args                []string
		mode                string
		mine, theirs, burst int
	}{
		{[]string{"reply"}, waid.RateLimitModeReply, 0, 0, 1},
		{[]string{"reply", "3"}, waid.RateLimitModeReply, 0, 0, 3},
		{[]string{"ratio", "2:3"}, waid.RateLimitModeRatio, 2, 3, 2},
		{[]string{"RATIO", "1:2", "4"}, waid.RateLimitModeRatio, 1, 2, 4},
	}
	for _, c := range good {
		s, err := parseRateLimitArgs(c.args, now)
		if err != nil {
			t.Fatalf("%v: %v", c.args, err)
		}
		if s.Mode != c.mode || s.Mine != c.mine || s.Theirs != c.theirs || s.Burst != c.burst || !s.EnabledAt.Time.Equal(now) {
			t.Fatalf("%v: got %+v", c.args, s)
		}
	}
	for _, args := range [][]string{
		{}, {"sometimes"}, {"reply", "0"}, {"reply", "1", "2"},
		{"ratio"}, {"ratio", "2"}, {"ratio", "2:0"}, {"ratio", "x:3"}, {"ratio", "2:3", "101"},
	} {
		if _, err := parseRateLimitArgs(args, now); err == nil {
			t.Fatalf("%v: expected an error", args)
		}
	}
}
