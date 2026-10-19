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

// OfflineHours is the homestacks per-chat offline window (see
// HOMESTACKS.md). It is stored inside PortalMetadata and edited with
// `!wa offline-hours`; nil means the chat has none.
//
// Start and End are minutes after midnight in the login's timezone. A window
// with Start > End wraps past midnight (22:00-08:00). Matrix messages sent
// inside the window are held and sent to WhatsApp at its end.
type OfflineHours struct {
	Start int `json:"start"`
	End   int `json:"end"`
}
