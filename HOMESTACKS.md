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
   `{{.CallType}}` and `{{.CallLink}}` — a freshly generated
   `<call_link_base_url>/<random>` link (Element Call by default);
3. posts a notice, as the user, into the Matrix portal with the same link, so the user is effectively
   "rung" on Matrix and can click straight into the call.

A caller only gets one text per `cooldown` (default 10m) no matter how often they retry; calls are still
declined and still produce a Matrix notice. Group calls are ignored unless `include_group_calls` is set.

Users tweak it per login from the bot DM:

```
!wa call-reply show
!wa call-reply set Hi {{.Name}}, no WhatsApp calls here — use {{.CallLink}}
!wa call-reply clear
!wa call-reply on | off
```

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
| `pkg/connector/callreply_config.go` | `CallAutoReplyConfig`, template parsing/validation, config upgrader |
| `pkg/connector/callreply_command.go` | `!wa call-reply` |
| `pkg/connector/callreply_test.go` | unit tests for the pure parts |
| `pkg/waid/callreply.go` | per-login settings stored in login metadata |

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
`commands.FullHandler`, `configupgrade`), the build or `go vet` step will say so; fix it in the `callreply*`
files rather than in upstream code.

## Building locally

```sh
go build -tags goolm ./...          # pure-Go olm, no libolm needed
go test -tags goolm ./pkg/...
docker build -t mautrix-whatsapp:dev .
```
