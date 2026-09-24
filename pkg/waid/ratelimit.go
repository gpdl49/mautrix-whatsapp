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

package waid

import "go.mau.fi/util/jsontime"

const (
	RateLimitModeReply = "reply"
	RateLimitModeRatio = "ratio"
)

// RateLimitSettings is the homestacks per-chat send limit (see HOMESTACKS.md).
// It is stored inside PortalMetadata and edited with `!wa rate-limit`; nil
// means the chat is not limited.
//
// Mode "reply": each message from the other side restores the credit to
// Burst. Mode "ratio": each one adds Mine/Theirs credit, capped at Burst.
// Every own message costs one credit and needs one to be sent. Only messages
// at or after EnabledAt count, so history from before the limit was set
// neither earns nor owes anything.
type RateLimitSettings struct {
	Mode      string             `json:"mode"`
	Mine      int                `json:"mine,omitempty"`
	Theirs    int                `json:"theirs,omitempty"`
	Burst     int                `json:"burst"`
	EnabledAt jsontime.UnixMilli `json:"enabled_at"`
}
