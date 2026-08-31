# Unimount Fork Features

Features added in the `unimountltd/cc-connect` fork on top of `chenhg5/cc-connect`.
Listed roughly newest-first within each section. Commit hashes link to fork history.

## Reliability

- **Single-instance guard via API socket probe** (`0b632de2`, `43a33e95`, `77a719d4`, `104c734e`)
  Before any platforms or engines start, the daemon dials `~/.cc-connect/api.sock`. If
  another bridge answers, it exits with the holder PID. Replaces an earlier flock-based
  lock that blocked legitimate `cc-connect send`/`cron`/`relay` subcommand spawns from
  Claude's Bash tool. `cc-connect version` is also now a real subcommand instead of
  falling through to a full daemon start.

- **Lock release before graceful close** (`c8ca73fe`)
  Platforms disconnect first, the singleton lock is released, then agent sessions are
  torn down. A replacement instance can serve immediately while the old process finishes
  its 130 s graceful shutdown.

- **Auto-retry on Anthropic rate-limit / overload** (`b38ee705`)
  When Claude returns `429 rate_limit_error` or `529 overloaded_error`, the engine waits
  and re-runs the turn on a fresh agent session. Budget: 30 attempts, paced "30 s then
  60 s each" (~30 min). Detection is structured — parses `error.type` from Anthropic's
  typed schema, not prose matching. Auth/validation failures still surface immediately.

- **Slack compact progress no longer freezes on transient errors** (`aeed70ff`)
  Slack rate limits, timeouts, and retryable 5xx now trigger a 3 s cooldown instead of
  permanently disabling the progress writer. The latest state is kept fresh during
  cooldown so the next update carries the current step. `*RateLimitedError.RetryAfter`
  is honoured. Elapsed-time ticker bumped 5 s → 10 s to halve `chat.update` volume on
  long tool runs.

- **Unconditional launchd KeepAlive** (`cd45e2c3`)
  launchd `KeepAlive` is unconditional (boolean `true`) rather than gated on
  `SuccessfulExit`. SIGKILL / OOM-kill / clean exits all trigger an immediate restart.
  Diverges from upstream's `SuccessfulExit=false` choice; we keep restart-on-everything
  since cc-connect is the daemon, not a finite job.

## Wakeup routing

- **SDK self-fired wakeup forwarding (UNI-12 v2)** (`e3722cbd`, `81f89798`, `474b1ed9`)
  Claude Code's in-process `ScheduleWakeup` fires events while no user turn is in
  flight. The engine detects these via a per-session in-turn channel-lock, treats
  them as orphan turns, renders each one to the messaging platform with a
  "Scheduled wakeup" header, and answers any permission prompts inline (auto-allow
  in `bypassPermissions`, deny otherwise). Replaces a v1 parallel-scheduler approach
  that double-fired. Follow-ups harden the channel-lock state machine, fix
  multi-workspace lookup, and bypass the lock for startup `system` events.

## Slack experience

- **Compact progress card** (`90552a4e`)
  Slack implements `MessageUpdater`, `PreviewStarter`, `PreviewCleaner`, and
  `ProgressStyleProvider`. Tool calls collapse into a single auto-updating message
  instead of one message per tool. Compact mode shows only the latest entry.

- **Direct file uploads** (`703fd793`)
  Slack platform implements `FileSender` via `UploadFileV2Context`, mirroring the
  existing image upload path.

- **Completion reactions** (`ca15ee0f`, `924eb1a1`)
  `:female-technologist:` while in progress, `:checkered_flag:` on success,
  `:woman-raising-hand:` on failure. Adds `CompletionReactor` and `EmojiReactor`
  interfaces in core so other platforms can opt in.

## Cross-platform progress UX

- **Friendly tool-call status lines** (`8ff6f8f9`, `5945fcbe`)
  Compact progress shows human-readable status like "📖 Reading file.go" and
  "⚡ Running: npm test" instead of raw tool output. Tool-call header changed from
  "🔧 Tool #N" to "👩🏻‍💻 Working on step N" (translated for all 5 languages).

- **Token usage indicator** (`9e7edf09`, `924eb1a1`)
  Replaces the minimal `[ctx: ~N%]` footer with turn duration, input/output token
  counts, and accurate context-window percentage. Parses
  `cache_creation_input_tokens` and `cache_read_input_tokens` from Claude Code result
  events. Compact progress card includes a usage summary on completion.

- **Elapsed-time ticker + deferred stop hint** (`54f6601a`, `924eb1a1`)
  Live elapsed-time on the compact progress card. Stop hint deferred for thinking
  events to reduce noise.

- **Per-model context-window % calculation** (`52d15aeb`)
  The `ctx ~%` indicator and telemetry `ContextPct` previously divided by a hardcoded
  200k. `contextWindowForModel()` now recognises Anthropic `[1m]` variants (1M),
  Gemini 1.5/2.5 (1M), GPT-5 / 4.1 (400k), defaulting to 200k. Plumbed through
  `usageIndicator`, compact progress `SetUsage`, and the telemetry collector. Active
  model resolves from session first, then agent.

  Since the v1.5.1 upstream merge the indicator is fed to `buildReplyFooter` as its
  context segment rather than appended to the response body. It therefore obeys the
  same `reply_footer` / `show_context_indicator` gating as every other status
  segment, and never leaks into upstream's "metadata only" send path as a standalone
  message. `buildReplyFooter` recognises the fork's bracketed group (not just
  upstream's `[ctx: ~N%]`) so it still leads the footer line.

## Session control

- **`/next <prompt>` + `--session-cmd /new --message "..."`** (`9d1e5a69`)
  A "reset + kick off" primitive. `/next` is sugar for `/new` followed by a first-turn
  message. The `--session-cmd` + `--message` combo atomically resets and injects the
  opening prompt — usable from batch scripts that iterate through issues. Restricted
  to `/new`, `/next`, `/switch` via `isChainableSessionCmd`.

- **Bare `stop` command** (`2261395c`)
  Type "stop" (case-insensitive) to abort a running agent session. Useful on platforms
  like Slack where `/` is intercepted as a slash-command prefix. Compact progress shows
  a "send stop to abort" hint that auto-strips on completion.

- **Bare `new session` + idle hint** (`eee05380`)
  Type "new session" (case-insensitive) to start fresh. After 4 hours of inactivity,
  a hint suggests starting a new session.

## Prompt engineering

- **Inject prompt ("keep in mind: …")** (`99f1cf86`, `d8476876`)
  Type `inject: <text>` (originally) or set persistent text that gets appended to every
  query in this channel. Shown as a 📌 header on the compact progress card. Persisted
  to disk in `projectstate.json` so it survives daemon restarts. Label was renamed
  from `[inject: ...]` → `[keep in mind: ...]` to reduce prompt-injection signal.

- **Sender name injection** (`924eb1a1`)
  `inject_sender` header now includes the sender display name so agents can identify
  who sent a message by name in shared channels.

## Telemetry (PostHog)

- **Per-turn PostHog collector + `cc-connect usage` CLI** (`d8476876`)
  Token usage, tool counts, duration, model, mode, and skill metadata reported per
  turn. Query metrics with HogQL via `cc-connect usage`. Configured under `[telemetry]`
  in `config.toml`.

- **Enabled by default with embedded write-only key** (`3026bad2`)
  Every deployment reports anonymous metrics out of the box; the embedded `phc_` key
  is write-only and cannot read or query. Opt out with `[telemetry] disabled = true`.
  Old `enabled = true/false` field is still honoured.

- **Channel / user breakdowns + `dashboard setup`** (`040465e1`)
  `chat_name` is threaded through the engine so PostHog events carry a human-readable
  channel name; `chat_id` is now populated for Slack (which never sets `ChannelKey`).
  `cc-connect dashboard setup` creates a PostHog dashboard with six HogQL insights:
  turns by channel/user, tokens by channel/user, daily trend, and a detail table.

## Updater & release channels

- **`--channel latest` + `--version` pin** (`be1b66a4`, `13af8751`, `c7425d41`)
  `cc-connect update` accepts `--channel latest` (rolling builds, formerly the `main`
  tag — renamed because it shadowed the `main` branch and broke pushes) and
  `--version vX.Y.Z` to pin a specific tag. Asset URLs resolve from the release
  object's assets list to support rolling tags with sha-suffixed filenames. Forks
  with no published stable release are handled gracefully.

- **Publish from `unimountltd` fork** (`e9e60d51`)
  Updater pulls release assets from `github.com/unimountltd/cc-connect`.

## Build & CI

- **Native ARM64 build workflow** (`9dd46fce`)
  Builds on `ubuntu-24.04-arm` and `macos-14` (Apple Silicon) runners. Uploaded as
  artifacts on every push/PR and attached to GitHub releases.

- **Trimmed release matrix** (`c4945772`)
  Release workflow ships `darwin/arm64` and `linux/arm64` only — the two platforms
  this fork actually deploys to.

- **Windows build tags for `run_as_user`** (`e6f9c6a6`)
  Adds the missing build tags so Windows builds don't drag in POSIX-only files.

- **`contents: write` permission for release uploads** (`6e73bc17`)
  Required for the release workflow to attach assets to GitHub releases under the
  default GITHUB_TOKEN scopes.

## Security

- **`SECURITY_REVIEW.md`** (`ab86d37b`)
  Comprehensive security review at the repo root, generated during the initial fork
  hardening pass.

## Upstream-merge maintenance

Not user-facing features, but deliberate fork tooling choices worth recording so
future upstream merges don't undo them.

- **Wholesale-take strategy for `engine_test.go`** (`2218e536`)
  On each upstream merge, `core/engine_test.go` is replaced verbatim with upstream's,
  then call sites for `processInteractiveEvents` are patched to pass the fork's
  trailing `telemetryMsgCtx{}` arg. Fork-only tests live in `core/engine_fork_test.go`
  so the wholesale-take stays clean.

- **Async-aware fork tests + targeted skip-list** (`c3ddf8d6`)
  `waitForInteractiveCleanup` helper handles upstream's now-async `/model` switch;
  the stub agent gets a mutex and `SentPrompts()` accessor for race-free assertions;
  one upstream compact-progress test is skipped as fork-divergent by design.

- **Lint cleanup of inherited upstream code** (`134644b7`)
  Silences errcheck on best-effort calls and removes dead helpers so CI passes.
  Mentioned here so it isn't re-added during a future merge.

- **`EffectiveDisplay` / `SaveDisplayConfig` signature adapters** (`4f8d68bd`)
  Stop-gap: pass `nil` / discard the new `mode` return value until the fork adopts
  upstream's full display-mode enum.

## Upstream convergence (v1.5.1 merge, 2026-08-31)

Features the fork had built independently that upstream later implemented too.
Recorded so nobody re-adds the fork's version on a future merge.

- **Per-workspace model persistence** — upstream shipped the same feature in #1372
  (`WorkspaceModelOverrides` + `persistWorkspaceModelOverride`, keyed off the agent's
  own `GetWorkDir()`). The fork's parallel `WorkspaceModelPref` implementation was
  dropped in favour of upstream's. The fork keeps only the half upstream lacks:
  reasoning effort, as `WorkspaceEffortOverrides` +
  `persistWorkspaceEffortOverride`, restored alongside the model in
  `getOrCreateWorkspaceAgent`. `/reasoning` and `/preset` write it.

- **`switchModelOnAgent` config gating** — the fork's fix (don't let a
  workspace-scoped `/model` rewrite `config.toml`'s project default) is upstream's
  behaviour now; the fork's `saveDefault` closure was removed.

- **`commandContextWS`** — upstream's `commandContextWithWorkspace` is the same
  helper. The fork's was renamed to upstream's name and its body kept.

- **Event token field names** — upstream's `CacheCreationInputTokens` /
  `CacheReadInputTokens` won over the fork's `CacheCreationTokens` /
  `CacheReadTokens`; renamed fork-wide. The fork's `ContextTokens` and `ErrorKind`
  remain as additions.

- **`lastCtxTokens`** — superseded by upstream's richer per-sub-call `cs.lastUsage`
  (`core.ContextUsage`). `Event.ContextTokens` is now sourced from it via
  `currentContextTokens()`, so the two can never disagree.

- **`EffectiveDisplay` / `SaveDisplayConfig` adapters** — the `4f8d68bd` stop-gap is
  retired. The fork now passes upstream's `mode` and `hideAgentFooter` through and
  sets `DisplayCfg.Mode` / `DisplayCfg.CardMode`.

- **Slack streaming preview** — upstream added `platform/slack/streaming.go`
  duplicating the fork's `SendPreviewStart` / `UpdateMessage`. The file was removed
  and upstream's two improvements (thread targeting, `MarkdownToSlackMrkdwn`) were
  folded into the fork's implementation, which keeps its `message_not_found`
  handling and rate-limit bounded retry.

### Upstream behaviour adopted over the fork's

- **`/model` no longer resets the session.** Upstream keeps the agent session ID
  across a model switch so the next `StartSession` runs
  `--resume <id> --model <new>` and the CLI restores context natively, instead of
  the fork's old clear-and-replay. Adopted for `/model`, its card action, and the
  async switch path; the fork's per-workspace scoping of those paths is kept.

- **`/ps` requires a live turn.** Upstream rejects `/ps` on an idle session
  because `agentSession.Send` would bypass the session lock and race the next
  normal message on the CLI's stdin. The fork's `ps:` bare-text shortcut still
  rewrites to `/ps`; it just inherits the guard.

### Merge hazard

- **`/next` must stay in `builtinCommands`.** `/next` is fork-only. Taking
  upstream's command list wholesale silently drops it and the command stops
  resolving — the dispatch `case "next"` alone is not enough, since `cmdID` is
  resolved from `builtinCommands` first. Same trap applies to any future
  fork-only command.

### Deliberate divergences kept

- **Compact-progress elapsed ticker.** The card takes a second preview edit on
  completion (the `· <elapsed>` suffix). Upstream's coalescing test was adapted to
  assert the final edit rather than an exact edit count.

- **Slack reply anchor.** Upstream now routes app-mention replies to the thread root.
  The fork keeps channel-root replies for mentions outside a thread (`e3f64ca5`).

- **Reply footer context format.** Upstream renders `[ctx: ~N%]`; the fork renders
  the full usage indicator. `tests/release_local/turn_contract` was adapted to assert
  the contract (exactly one indicator, at the tail) rather than upstream's literal
  string — see `countCtxIndicators` / `hasCtxIndicator` in that file.

- **`inTurn` is a counter, not a bool.** The fork's wakeup routing used a boolean
  "a user turn is in flight" flag. Upstream's keep-alive test pipelines 100+ sends
  before draining, which the boolean mis-handled (turns 2+ were misrouted to
  `orphanEvents`). It is now an `atomic.Int32` incremented per `Send` and decremented
  on each terminal event; `> 0` means a user turn owns the stream.

- **Compaction is not an error.** The fork's `handleResult` routes any non-`success`
  subtype to `EventError` for retry classification. Upstream's mid-turn compaction
  arrives as `subtype: "compact"`/`"compaction"`, so it is explicitly excluded —
  otherwise every auto-compact would abort the turn.
