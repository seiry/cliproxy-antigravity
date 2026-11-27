# Fork notes

**Archived 2026-11-27.** Upstream v0.6.0 merged the fork's work; see the notice at the top of
`README.md`. The notes below are the fork's history up to 0.4.0-acp.1.

This is `seiry/cliproxy-antigravity`, a fork of
[adeebahmad01/cliproxy-antigravity](https://github.com/adeebahmad01/cliproxy-antigravity). It keeps
upstream's plugin and adds a resident `agy acp` execution mode. This file records what the fork changes
and, for every upstream release, what was merged and what was left out, so the next upstream merge
starts from known decisions.

## Following upstream

- Only `main` exists on the fork's remote. Upstream releases are merged with a real merge commit
  (`git merge upstream/main` or the release tag), never cherry-picked, so git knows which upstream
  commits are already in.
- A feature the fork does not adopt is still merged, then reverted or deleted inside that merge commit,
  and listed below. When a later upstream release touches the same files, the merge shows a conflict
  (or a modify/delete conflict for deleted files) and this file says how it was decided last time.
- Versions read `<upstream version>-acp.<n>`, for example `0.4.0-acp.1`. CLIProxyAPI compares the
  prerelease part as text (`acp.10` sorts below `acp.9`), so move the previous `.so` out of the plugin
  directory on every swap.

## What the fork adds

- `mode: acp` (the default): one resident `agy acp` process per account home, a session per request,
  and continuation of an earlier session when a request repeats its transcript (prompt cache hits).
  `mode: print` is upstream's `agy -p` path. A request with a `conversation_id` always takes the print
  path.
- Provider key `agy-acp` instead of `agy`; model IDs keep `agy/`. Auth files typed `agy` are rewritten.
- Models are registered per enabled auth (`model.for_auth`, executor model scope `oauth`). There is no
  model router and no self route: without an enabled agy auth file, agy serves nothing.
- Auth files deleted in the panel stay deleted (`.cpa-auth-file-issued` marker in the account home).
- Streams always end with a usage chunk; tool call arguments are sent as single-line JSON; audio, video
  and file attachments reach agy in ACP mode.
- The accounts page is served with `PROVIDER` set to `agy-acp` (`login.go` rewrites upstream's
  `const PROVIDER = 'agy';`; `TestAccountsManagementRoutes` fails if that line changes upstream).

Tests: `go test ./...` with an empty `HOME` (otherwise it starts the real local agy), `make smoke` for
the ABI contract in print mode, and `scripts/e2e_acp.py` against a CLIProxyAPI that loads the plugin.

## Upstream releases

### 0.4.0 (merged as 0.4.0-acp.1, 2026-11-24)

Taken:

- Chat Completions stream framing: plain JSON chunks on `/v1/chat/completions` and `/v1/completions`,
  `data:` framing and `[DONE]` elsewhere, and no role chunk before the first real output. Ported to
  both the print path (`agy.go`) and the ACP path (`acp_exec.go`) through `chatStream` in `openai.go`.
  CLIProxyAPI v8.0.80 already strips plugin framing for same-format streams, so on that host this
  changes nothing visible; it matters on hosts without that normalization.
- The new accounts page (`accounts_page.html`): no second key prompt, panel bridge
  (`agy-accounts/v1`) or the panel's remembered login, hide-email also hides filenames and homes. The
  only fork edit is the "How multiple accounts work" paragraph, which describes ACP continuation.
- `Cache-Control: no-store` on the accounts API, the release workflow changes, the private-data
  assertions in `login_test.go`, and HOME isolation in `scripts/abi_smoke.py`.

Not taken (account-bound conversations):

- `conversations.go` and `conversations_test.go` are deleted: signed `agy.v1.` conversation IDs, the
  `conversation.key` file, `X-AGY-Continue-On-Another-Account` and `agy_replay_summary`.
- The `scheduler` (`scheduler.pick`, `scheduler_across_priorities`) and `request_interceptor`
  (`request.intercept_before/after`) capabilities, the `agy_session_home` auth attribute in `login.go`,
  and the conversation-aware `routeModel`.
- The `prepareAgyExecution` / `buildChatPrompt` refactor in `agy.go`, `openai.go` and `plugin.go`,
  which exists to carry the signed IDs. The fork keeps `buildPrompt`.

Why: production traffic runs in ACP mode, where continuation comes from the transcript fingerprint and
account stickiness from CLIProxyAPI's `routing.session-affinity`; the signed IDs only serve print-mode
clients that store and resend an ID. CLIProxyAPI also activates a single scheduler plugin, and the
fork's per-auth model registration (`oauth` scope) was never tested with upstream's scheduler.
Revisit if a client needs explicit cross-account handoff.
