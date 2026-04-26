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
