# homestacks fork of mautrix-whatsapp

This is [gpdl49/mautrix-whatsapp](https://github.com/gpdl49/mautrix-whatsapp), a fork of
[mautrix/whatsapp](https://github.com/mautrix/whatsapp) used by the
[homestacks](https://github.com/vidurb/homestacks) GitOps repo. Read this file first when an
upstream merge conflicts.

## What the fork adds

**Call auto-reply.** WhatsApp calls cannot be answered through the bridge. With
`call_auto_reply.enabled: true` the bridge, on every incoming 1:1 call:

1. declines the call (`whatsmeow.Client.RejectCall`), so the caller hears "declined" instead of ringing out;
2. texts the caller a configurable message, rendered from a Go template with `{{.Name}}`, `{{.Phone}}`,
   `{{.CallType}}`, `{{.Account}}` (the user's own number that was called) and `{{.CallLink}}` — an Element Call link to a Matrix room created for this
   call (see **Call links** below);
3. posts a notice, as the user, into the Matrix portal with the same link, so the user is effectively
   "rung" on Matrix and can click straight into the call.

A caller only gets one text per `cooldown` (default 10m) no matter how often they retry; calls are still
declined and still produce a Matrix notice. Group calls are ignored unless `include_group_calls` is set.

### Call links

Element Call **never creates a room from a link**; it only joins a room whose ID is already in the
URL fragment. So `{{.CallLink}}` is only useful if a real room exists behind it, and the bridge
creates one per call: public, unencrypted, `history_visibility: joined`, discarded after `room_ttl`.

Three things about that room are load-bearing, and each fails in a way that looks like something
else:

- **Members must be allowed to publish their own call membership.** A room created with
  `preset: public_chat` gets `state_default: 50` while a joined caller has `users_default: 0`, so
  Element Call is refused when it writes its membership state and tears the call down within
  milliseconds. From the browser this is indistinguishable from broken media. Both event names are
  granted (`m.rtc.member` and the legacy `org.matrix.msc3401.call.member`) because current clients
  write the legacy one, and granting only the newer name looks right and changes nothing.
- **The user is joined, not invited.** The ring only reaches a joined member, so the bridge joins
  them through the double puppet. Without double puppeting it falls back to an invite and logs a
  warning — calls will not ring until the invite is accepted, which defeats the point.
- **`guest_homeserver_url` is required.** Element Call registers the caller a real account via
  `/register`; left to itself it uses a guest server that federates with nothing, so the link loads
  and then fails exactly when the call starts. The bridge refuses to start rather than hand out
  links that cannot work. It needs a homeserver with open registration that federates with wherever
  the call rooms live — not your main homeserver.

`sendNotificationType=ring` in the link makes Element Call emit the ring notification itself once
the caller is actually in the call, so the bridge needs no ring code and the user is only rung when
there is something to answer.

The homestacks repo's `docs/matrix-call-links.md` has the full derivation, including the dead ends.

Users tweak it per login from the bot DM:

```
!wa call-reply show
!wa call-reply set Hi {{.Name}}, no WhatsApp calls here — use {{.CallLink}}
!wa call-reply clear
!wa call-reply on | off
```

With several WhatsApp logins on one Matrix account, each login declines, texts and rings on its own,
with its own cooldown and settings. The command then needs to know which login it is for: name it
first (`!wa call-reply 15550000001 show`, with or without `+`), or run the command in one of that
login's chats. With a single login nothing changes. Without either it refuses and lists the logins
rather than guess.

Set upstream's `call_start_notices: false` alongside, otherwise both the upstream "Incoming call" notice
and ours appear.

## Branches and tags

| Ref | Meaning |
|---|---|
| `main` | Untouched mirror of upstream `main`. Never commit here. |
| `homestacks` | Upstream release tag + fork commits. Advanced by **merging** (`--no-ff`) each new upstream tag, never rebased, so submodule pointers in homestacks stay valid. |
| `vX.Y.Z-hs.N` | Release tag: upstream tag `vX.Y.Z` plus fork revision `N`. `N` bumps only when fork code changes on the same upstream base. |

Images: `ghcr.io/gpdl49/mautrix-whatsapp:<tag>` (and `:homestacks` floating), built from upstream's
unmodified `Dockerfile` by `.github/workflows/publish.yml`.

## Automation

- `.github/workflows/upstream-sync.yml` — daily. Fetches upstream tags, merges the newest release tag not
  yet in `homestacks`, builds, vets and tests, pushes, tags `<tag>-hs.1`, and calls `publish.yml`. On a merge
  conflict it opens an issue titled `Upstream merge conflict: <tag>` and stops; resolve by hand (below).
- `.github/workflows/publish.yml` — builds and pushes the image for a `v*-hs.*` tag (on tag push, on
  dispatch, or called by the sync workflow).
- `.github/workflows/go.yml` — upstream's lint workflow, kept as is.

## Where the fork touches upstream code

All feature code lives in **new files**. Every hook into an upstream file is a single line tagged
`// homestacks:` — `grep -rn 'homestacks:' pkg/` lists them all.

| Upstream file | Hook |
|---|---|
| `pkg/connector/client.go` | registers `handleCallAutoReply` as a second whatsmeow event handler, right after `handleWAEvent` |
| `pkg/connector/connector.go` | adds `cmdCallReply` to the command list |
| `pkg/connector/config.go` | `CallAutoReply` field on `Config`; `postProcess()` call at the end of `PostProcess`; `upgradeCallAutoReplyConfig(helper)` in `upgradeConfig`; `call_auto_reply` block entry in `GetConfig` |
| `pkg/connector/example-config.yaml` | `call_auto_reply:` section appended at the end |
| `pkg/waid/dbmeta.go` | `CallAutoReply` field on `UserLoginMetadata` |

New files:

| File | Purpose |
|---|---|
| `pkg/connector/callreply.go` | event handler, decline + text + Matrix notice, dedupe/cooldown tracker |
| `pkg/connector/callroom.go` | per-call Matrix room, power levels, double-puppet join, link builder, TTL cleanup |
| `pkg/connector/callreply_config.go` | `CallAutoReplyConfig`, template parsing/validation, config upgrader |
| `pkg/connector/callreply_command.go` | `!wa call-reply` |
| `pkg/connector/callreply_test.go` | unit tests for the pure parts |
| `pkg/waid/callreply.go` | per-login settings stored in login metadata |

## Upstreaming

The fork is two features that happen to share a config section, and only one of
them is plausible upstream:

| | Upstreamable | Why |
|---|---|---|
| Decline incoming calls + text the caller | **Yes** | Self-contained, no assumptions about anyone's infrastructure. WhatsApp calls genuinely cannot be answered through the bridge, so every user of this bridge has the problem. |
| Per-call Matrix room and Element Call link | **No** | Needs a second homeserver with open registration, federation between it and your own, and an SFU. Far too opinionated to ask upstream to carry. |

The split is marked in code: `grep -rn 'call-link:' pkg/` lists every point that
belongs to the second feature. Four hits, and they are not all the same kind —
two in `callreply.go` are the actual seams, both `if` blocks inside
`autoReplyToCall` that can be deleted outright; two in `callreply_config.go`
mark the config fields and the `CallLink` placeholder that go with them.

To prepare an upstream PR, on a branch off upstream `main`:

1. Take `callreply.go`, `callreply_config.go`, `callreply_command.go`,
   `callreply_test.go` and `pkg/waid/callreply.go` as they are.
2. Delete `callroom.go`, and delete the two `call-link:` blocks in
   `autoReplyToCall` along with the `callRoomID` variable they share.
3. Drop `CallLink` from `callReplyTemplateData`, and the four call-link fields
   (`CallLinkBaseURL`, `GuestHomeserverURL`, `ViaServers`, `RoomTTL`) from
   `CallAutoReplyConfig`, their `helper.Copy` lines, and the
   `guest_homeserver_url` check in `postProcess`.
4. Change the default message, which currently ends in `{{.CallLink}}`. Something
   like `"Hi {{.Name}}, I can't receive WhatsApp calls on this number."`
5. Trim `example-config.yaml` to the surviving keys: `enabled`, `message`,
   `cooldown`, `include_group_calls`.
6. Drop `TestBuildCallLink` and `TestCallRoomMemberEventsArePermitted`, and the
   call-link assertions in `TestCallAutoReplyConfigPostProcess`. The rest of the
   tests apply unchanged.
7. Keep the `// homestacks:` hook comments out of it — rename or drop them, they
   are a marker for this fork's merge conflicts and mean nothing upstream.

What is left is: decline the call, render a template, send it, post a notice,
with a per-caller cooldown and a `!wa call-reply` command. Nothing in it refers
to Element Call, Matrix RTC, or a guest homeserver.

Two things worth raising in such a PR rather than hiding: it adds a second
whatsmeow event handler next to `handleWAEvent` (deliberately, so upstream call
handling stays untouched), and it wants `call_start_notices: false` alongside or
users get two notices per call.

## Resolving an upstream merge conflict by hand

```sh
git checkout homestacks
git fetch upstream --tags
git merge --no-ff vX.Y.Z
# fix conflicts — the fork's side is always one `// homestacks:` line or one of the new files above
go build -tags goolm ./... && go vet -tags goolm ./... && go test -tags goolm ./pkg/...
git commit
git push origin homestacks
git tag vX.Y.Z-hs.1 && git push origin vX.Y.Z-hs.1   # publish.yml builds the image
```

If upstream changed something the feature relies on (`RejectCall`, `BasicCallMeta`, `QueueRemoteEvent`,
`commands.FullHandler`, `configupgrade`, `MatrixAPI.CreateRoom`/`EnsureJoined`/`DeleteRoom`,
`User.DoublePuppet`), the build or `go vet` step will say so; fix it in the `callreply*`/`callroom.go`
files rather than in upstream code.

## Building locally

```sh
go build -tags goolm ./...          # pure-Go olm, no libolm needed
go test -tags goolm ./pkg/...
docker build -t mautrix-whatsapp:dev .
```
