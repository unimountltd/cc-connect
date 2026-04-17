package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// compactStatusMaxLen caps the total rendered status line so it stays on a
// single line and the chat below does not jump up/down on each update.
const compactStatusMaxLen = 200

const (
	progressStyleLegacy  = "legacy"
	progressStyleCompact = "compact"
	progressStyleCard    = "card"

	// ProgressCardPayloadPrefix marks a structured payload for card-style progress.
	ProgressCardPayloadPrefix = "__cc_connect_progress_card_v1__:"

	// Keep a margin below platform hard limit for markdown wrappers/code fences.
	compactProgressMaxChars = maxPlatformMessageLen - 200

	// Bound each platform progress-card API call so a hung upstream request
	// does not block the whole turn forever.
	compactProgressAPITimeout = 15 * time.Second

	// How often the elapsed-time ticker refreshes the compact status line.
	// Kept moderately coarse so busy turns do not burn the platform's
	// chat.update rate budget (e.g. Slack tier 3 ≈ 1/sec per channel).
	compactElapsedTickInterval = 10 * time.Second

	// After a transient update failure (rate limit, timeout, 5xx) skip
	// attempts for this long so we do not hammer the platform while it is
	// asking us to back off.
	compactProgressCooldown = 3 * time.Second

	// Show the "send stop to abort" hint only after this duration.
	compactStopHintDelay = 10 * time.Second
)

type ProgressCardState string

const (
	ProgressCardStateRunning   ProgressCardState = "running"
	ProgressCardStateCompleted ProgressCardState = "completed"
	ProgressCardStateFailed    ProgressCardState = "failed"
)

type ProgressCardEntryKind string

const (
	ProgressEntryInfo       ProgressCardEntryKind = "info"
	ProgressEntryThinking   ProgressCardEntryKind = "thinking"
	ProgressEntryToolUse    ProgressCardEntryKind = "tool_use"
	ProgressEntryToolResult ProgressCardEntryKind = "tool_result"
	ProgressEntryError      ProgressCardEntryKind = "error"
)

type ProgressCardEntry struct {
	Kind     ProgressCardEntryKind `json:"kind"`
	Text     string                `json:"text"`
	Tool     string                `json:"tool,omitempty"`
	Status   string                `json:"status,omitempty"`
	ExitCode *int                  `json:"exit_code,omitempty"`
	Success  *bool                 `json:"success,omitempty"`
}

// ProgressCardPayload carries structured progress entries for platforms that
// render custom progress cards.
type ProgressCardPayload struct {
	Version   int                 `json:"version,omitempty"`
	Agent     string              `json:"agent,omitempty"`
	Lang      string              `json:"lang,omitempty"`
	State     ProgressCardState   `json:"state,omitempty"`
	Entries   []string            `json:"entries,omitempty"` // legacy fallback
	Items     []ProgressCardEntry `json:"items,omitempty"`   // ordered typed events
	Truncated bool                `json:"truncated"`
}

// BuildProgressCardPayload encodes progress entries into a transport string.
// This legacy builder keeps compatibility with old callers that only send text.
func BuildProgressCardPayload(entries []string, truncated bool) string {
	cleaned := make([]string, 0, len(entries))
	for _, entry := range entries {
		entry = strings.TrimSpace(entry)
		if entry != "" {
			cleaned = append(cleaned, entry)
		}
	}
	if len(cleaned) == 0 {
		return ""
	}
	payload := ProgressCardPayload{
		Entries:   cleaned,
		Truncated: truncated,
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return ""
	}
	return ProgressCardPayloadPrefix + string(b)
}

// BuildProgressCardPayloadV2 encodes ordered typed progress events.
func BuildProgressCardPayloadV2(items []ProgressCardEntry, truncated bool, agent string, lang Language, state ProgressCardState) string {
	cleaned := make([]ProgressCardEntry, 0, len(items))
	for _, item := range items {
		text := strings.TrimSpace(item.Text)
		if text == "" {
			continue
		}
		kind := item.Kind
		if kind == "" {
			kind = ProgressEntryInfo
		}
		cleaned = append(cleaned, ProgressCardEntry{
			Kind:     kind,
			Text:     text,
			Tool:     strings.TrimSpace(item.Tool),
			Status:   strings.TrimSpace(item.Status),
			ExitCode: item.ExitCode,
			Success:  item.Success,
		})
	}
	if len(cleaned) == 0 {
		return ""
	}
	if state == "" {
		state = ProgressCardStateRunning
	}
	payload := ProgressCardPayload{
		Version:   2,
		Agent:     strings.TrimSpace(agent),
		Lang:      string(lang),
		State:     state,
		Items:     cleaned,
		Truncated: truncated,
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return ""
	}
	return ProgressCardPayloadPrefix + string(b)
}

// ParseProgressCardPayload decodes a structured progress payload.
func ParseProgressCardPayload(content string) (*ProgressCardPayload, bool) {
	if !strings.HasPrefix(content, ProgressCardPayloadPrefix) {
		return nil, false
	}
	raw := strings.TrimPrefix(content, ProgressCardPayloadPrefix)
	var payload ProgressCardPayload
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		return nil, false
	}
	legacy := make([]string, 0, len(payload.Entries))
	for _, entry := range payload.Entries {
		entry = strings.TrimSpace(entry)
		if entry != "" {
			legacy = append(legacy, entry)
		}
	}
	items := make([]ProgressCardEntry, 0, len(payload.Items))
	for _, item := range payload.Items {
		item.Text = strings.TrimSpace(item.Text)
		item.Tool = strings.TrimSpace(item.Tool)
		item.Status = strings.TrimSpace(item.Status)
		if item.Text == "" {
			continue
		}
		if item.Kind == "" {
			item.Kind = ProgressEntryInfo
		}
		items = append(items, item)
	}
	if len(items) == 0 && len(legacy) > 0 {
		for _, entry := range legacy {
			items = append(items, ProgressCardEntry{
				Kind: inferLegacyEntryKind(entry),
				Text: entry,
			})
		}
	}
	if len(items) == 0 && len(legacy) == 0 {
		return nil, false
	}
	if payload.State == "" {
		payload.State = ProgressCardStateRunning
	}
	payload.Items = items
	payload.Entries = legacy
	if len(payload.Entries) == 0 && len(payload.Items) > 0 {
		payload.Entries = make([]string, 0, len(payload.Items))
		for _, item := range payload.Items {
			payload.Entries = append(payload.Entries, item.Text)
		}
	}
	return &payload, true
}

func inferLegacyEntryKind(entry string) ProgressCardEntryKind {
	switch {
	case strings.HasPrefix(entry, "💭"):
		return ProgressEntryThinking
	case strings.HasPrefix(entry, "🔧"), strings.HasPrefix(entry, "👩🏻\u200d💻"), strings.Contains(entry, "**Tool #"), strings.Contains(entry, "**Working on step"):
		return ProgressEntryToolUse
	case strings.HasPrefix(entry, "🧾"):
		return ProgressEntryToolResult
	case strings.HasPrefix(entry, "❌"):
		return ProgressEntryError
	default:
		return ProgressEntryInfo
	}
}

// compactProgressWriter coalesces intermediate progress (thinking/tool-use)
// into one editable message for platforms that support message updates.
type compactProgressWriter struct {
	ctx       context.Context
	platform  Platform
	replyCtx  any
	transform func(string) string

	starter PreviewStarter
	updater MessageUpdater
	handle  any

	enabled    bool
	failed     bool
	style      string
	usePayload bool

	workDir      string // stripped from tool-use paths for display
	injectHeader string // custom inject prompt shown at top of progress card

	mu             sync.Mutex
	content        string
	baseContent    string // content without stop hint, used for final update
	entries        []string
	items          []ProgressCardEntry
	state          ProgressCardState
	agentName      string
	lang           Language
	truncated      bool
	lastSent       string
	maxEntries     int
	toolStartAt    time.Time             // when current tool_use/thinking started
	toolBaseStatus string                // rendered status without elapsed/hint
	activeKind     ProgressCardEntryKind // kind of the current active line
	frozenLines    []string              // completed progress lines with frozen elapsed
	tickerStop     chan struct{}
	usageSuffix    string    // usage indicator to include in final progress card
	cooldownUntil  time.Time // skip UpdateMessage calls until this moment (set on transient errors)
}

// isTransientUpdateErr reports whether an UpdateMessage error is expected to
// clear up on its own — rate limits, server timeouts, transient 5xx. The goal
// is to avoid disabling the progress card for the rest of the turn when a
// single call hits Slack's rate limit or a 15 s API deadline.
//
// Permanent failures (auth revoked, message deleted, invalid channel) still
// flip w.failed so we stop hammering a dead handle.
func isTransientUpdateErr(err error) bool {
	if err == nil {
		return false
	}
	// Caller-side timeout: the 15s API-call budget expired. Almost always a
	// slow upstream response, not a permanent failure.
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	// Platform-returned errors that advertise retryability (e.g.
	// slack-go's *RateLimitedError satisfies this).
	var retryable interface{ Retryable() bool }
	if errors.As(err, &retryable) && retryable.Retryable() {
		return true
	}
	// Heuristic fallback: recognise common rate-limit / server-error
	// wording so platforms without a typed error still benefit.
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "rate limit"),
		strings.Contains(msg, "rate_limited"),
		strings.Contains(msg, "ratelimited"),
		strings.Contains(msg, "too many requests"),
		strings.Contains(msg, "status 429"),
		strings.Contains(msg, "status 500"),
		strings.Contains(msg, "status 502"),
		strings.Contains(msg, "status 503"),
		strings.Contains(msg, "status 504"),
		strings.Contains(msg, "service_unavailable"),
		strings.Contains(msg, "temporarily unavailable"):
		return true
	}
	return false
}

// inCooldown reports whether we should skip sending an update right now
// because a recent transient failure asked us to back off. Must be called
// with w.mu held.
func (w *compactProgressWriter) inCooldown() bool {
	return !w.cooldownUntil.IsZero() && time.Now().Before(w.cooldownUntil)
}

// noteTransientErrLocked records that an UpdateMessage call failed
// transiently and sets a short cooldown before the next attempt. Must be
// called with w.mu held.
func (w *compactProgressWriter) noteTransientErrLocked() {
	w.cooldownUntil = time.Now().Add(compactProgressCooldown)
}

func normalizeProgressStyle(style string) string {
	switch strings.ToLower(strings.TrimSpace(style)) {
	case "", progressStyleLegacy:
		return progressStyleLegacy
	case progressStyleCompact:
		return progressStyleCompact
	case progressStyleCard:
		return progressStyleCard
	default:
		return progressStyleLegacy
	}
}

func progressStyleForPlatform(p Platform) string {
	ps := progressStyleLegacy
	if sp, ok := p.(ProgressStyleProvider); ok {
		ps = normalizeProgressStyle(sp.ProgressStyle())
	}
	return ps
}

// SuppressStandaloneToolResultEvent is true when a platform opts into progress
// styling (ProgressStyleProvider) but uses legacy mode. In that case tool_use
// lines are still shown, but a separate chat message for EventToolResult is
// skipped to avoid duplicate noise (e.g. Codex structured tool results on Feishu).
// Platforms without ProgressStyleProvider keep showing standalone tool results.
func SuppressStandaloneToolResultEvent(p Platform) bool {
	_, ok := p.(ProgressStyleProvider)
	if !ok {
		return false
	}
	return progressStyleForPlatform(p) == progressStyleLegacy
}

func newCompactProgressWriter(ctx context.Context, p Platform, replyCtx any, agentName string, lang Language, transform func(string) string, workDir string, injectHeader string) *compactProgressWriter {
	w := &compactProgressWriter{
		ctx:          ctx,
		platform:     p,
		replyCtx:     replyCtx,
		transform:    transform,
		style:        progressStyleForPlatform(p),
		state:        ProgressCardStateRunning,
		agentName:    normalizeProgressAgentLabel(agentName),
		lang:         lang,
		workDir:      workDir,
		injectHeader: injectHeader,
		maxEntries:   10,
	}
	if w.style != progressStyleCompact && w.style != progressStyleCard {
		slog.Debug("progress writer disabled: unsupported style", "platform", p.Name(), "style", w.style)
		return w
	}
	updater, ok := p.(MessageUpdater)
	if !ok {
		slog.Debug("progress writer disabled: platform has no MessageUpdater", "platform", p.Name(), "style", w.style)
		return w
	}
	w.enabled = true
	w.updater = updater
	if starter, ok := p.(PreviewStarter); ok {
		w.starter = starter
	}
	if w.style == progressStyleCard {
		if cap, ok := p.(ProgressCardPayloadSupport); ok && cap.SupportsProgressCardPayload() {
			w.usePayload = true
		}
	}
	slog.Debug("progress writer enabled", "platform", p.Name(), "style", w.style, "use_payload", w.usePayload)
	return w
}

func normalizeProgressAgentLabel(name string) string {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "", "agent":
		return "Agent"
	case "codex":
		return "Codex"
	case "claudecode", "claude-code", "cc":
		return "CC"
	case "gemini":
		return "Gemini"
	case "cursor":
		return "Cursor"
	case "qoder":
		return "Qoder"
	case "iflow":
		return "iFlow"
	case "opencode":
		return "OpenCode"
	case "pi":
		return "PI"
	default:
		n := strings.TrimSpace(name)
		if n == "" {
			return "Agent"
		}
		return strings.ToUpper(n[:1]) + n[1:]
	}
}

// Append appends one progress item and updates the in-place message.
// Returns true when compact rendering handled this item; false means caller
// should fallback to legacy per-event send.
func (w *compactProgressWriter) Append(item string) bool {
	return w.AppendEvent(ProgressEntryInfo, item, "", item)
}

// AppendEvent appends one typed progress event and updates the in-place message.
// fallback is used for compact/plain rendering when style-specific rendering is not available.
func (w *compactProgressWriter) AppendEvent(kind ProgressCardEntryKind, text string, tool string, fallback string) bool {
	return w.AppendStructured(ProgressCardEntry{
		Kind: kind,
		Text: text,
		Tool: tool,
	}, fallback)
}

// AppendStructured appends one structured progress event and updates the in-place message.
func (w *compactProgressWriter) AppendStructured(item ProgressCardEntry, fallback string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.enabled || w.failed {
		return false
	}
	text := strings.TrimSpace(item.Text)
	fallback = strings.TrimSpace(fallback)
	if text == "" && fallback == "" {
		return true
	}
	if text == "" {
		text = fallback
	}
	if fallback == "" {
		fallback = text
	}
	switch item.Kind {
	case ProgressEntryThinking, ProgressEntryError, ProgressEntryInfo:
		if w.transform != nil {
			text = w.transform(text)
			fallback = w.transform(fallback)
		}
	case ProgressEntryToolUse:
		if w.workDir != "" {
			text = stripWorkDirPrefix(text, w.workDir)
		}
	}
	kind := item.Kind
	if kind == "" {
		kind = ProgressEntryInfo
	}
	item.Kind = kind
	item.Text = text
	item.Tool = strings.TrimSpace(item.Tool)
	item.Status = strings.TrimSpace(item.Status)

	switch w.style {
	case progressStyleCard:
		w.items = append(w.items, item)
		w.entries = append(w.entries, fallback)
		truncated := false
		if w.maxEntries > 0 && len(w.items) > w.maxEntries {
			w.items = w.items[len(w.items)-w.maxEntries:]
			if len(w.entries) > w.maxEntries {
				w.entries = w.entries[len(w.entries)-w.maxEntries:]
			}
			truncated = true
		} else if w.maxEntries > 0 && len(w.entries) > w.maxEntries {
			w.entries = w.entries[len(w.entries)-w.maxEntries:]
			truncated = true
		}
		w.truncated = truncated
		if w.usePayload {
			w.content = BuildProgressCardPayloadV2(w.items, w.truncated, w.agentName, w.lang, w.state)
			if w.content == "" {
				slog.Warn("progress writer: failed to build structured payload", "platform", w.platform.Name())
				w.failed = true
				return false
			}
		} else {
			w.content = renderCardProgressMarkdownFallback(w.entries, truncated)
			w.content = trimCompactProgressText(w.content, compactProgressMaxChars)
		}
	default:
		// ── compact style ──
		// Accumulate all progress events as lines in a single code block.
		// Freeze the current active line (with elapsed) before starting a new one.
		w.stopTickerLocked()
		w.freezeActiveLocked()

		if item.Kind == ProgressEntryToolResult {
			// Tool completed — the active line was already frozen above.
			w.content = w.buildCompactBlock("")
			break // fall through to send update below
		}

		// Build the new active line.
		var activeLine string
		if friendly := renderCompactStatus(item, w.lang); friendly != "" {
			activeLine = friendly
		} else {
			activeLine = fallback
		}

		w.baseContent = activeLine
		w.activeKind = item.Kind

		if item.Kind == ProgressEntryToolUse || item.Kind == ProgressEntryThinking {
			// Timed events: elapsed + stop hint are deferred to the ticker.
			w.toolStartAt = time.Now()
			w.toolBaseStatus = activeLine
			w.startToolTickerLocked()
		} else {
			// Non-timed events: show stop hint immediately on the active line.
			w.toolStartAt = time.Time{}
			w.toolBaseStatus = ""
			if hint := translateMsg(w.lang, MsgCompactStopHint); hint != "" {
				activeLine += " · " + hint
			}
		}

		w.content = w.buildCompactBlock(activeLine)
	}

	if w.content == w.lastSent {
		return true
	}

	if w.handle == nil {
		if w.starter != nil {
			callCtx, cancel := w.withAPITimeout()
			handle, err := w.starter.SendPreviewStart(callCtx, w.replyCtx, w.content)
			cancel()
			if err != nil || handle == nil {
				slog.Warn("progress writer: SendPreviewStart failed", "platform", w.platform.Name(), "style", w.style, "error", err, "handle_nil", handle == nil)
				w.failed = true
				return false
			}
			w.handle = handle
			w.lastSent = w.content
			return true
		}
		callCtx, cancel := w.withAPITimeout()
		err := w.platform.Send(callCtx, w.replyCtx, w.content)
		cancel()
		if err != nil {
			slog.Warn("progress writer: initial Send failed", "platform", w.platform.Name(), "style", w.style, "error", err)
			w.failed = true
			return false
		}
		w.handle = w.replyCtx
		w.lastSent = w.content
		return true
	}

	if w.inCooldown() {
		// Platform asked us to back off recently. Leave w.content fresh so
		// the next successful update carries the latest state; don't mark
		// the writer failed.
		return true
	}

	callCtx, cancel := w.withAPITimeout()
	err := w.updater.UpdateMessage(callCtx, w.handle, w.content)
	cancel()
	if err != nil {
		if isTransientUpdateErr(err) {
			slog.Warn("progress writer: UpdateMessage transient failure; backing off", "platform", w.platform.Name(), "style", w.style, "error", err, "cooldown", compactProgressCooldown)
			w.noteTransientErrLocked()
			return true
		}
		slog.Warn("progress writer: UpdateMessage failed", "platform", w.platform.Name(), "style", w.style, "error", err)
		w.failed = true
		return false
	}
	w.cooldownUntil = time.Time{}
	w.lastSent = w.content
	return true
}

// SetUsage stores token usage stats to include in the final compact progress card.
func (w *compactProgressWriter) SetUsage(totalInput, outputTokens, contextTokens int, duration time.Duration) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.style != progressStyleCompact || !w.enabled || w.failed {
		return
	}
	w.usageSuffix = usageIndicatorCompact(totalInput, outputTokens, contextTokens, duration)
}

// UsageHandled returns true when the compact progress card includes the usage indicator,
// so the caller can skip appending it to the response text.
func (w *compactProgressWriter) UsageHandled() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.style == progressStyleCompact && w.enabled && !w.failed && w.handle != nil && w.usageSuffix != ""
}

// usageIndicatorCompact returns an inline usage string like "[23s · 65.6k in · 461 out · ctx ~6%]"
// without the leading newline (for embedding in progress card).
func usageIndicatorCompact(totalInput, outputTokens, contextTokens int, duration time.Duration) string {
	var parts []string
	if duration >= time.Second {
		parts = append(parts, formatDuration(duration))
	}
	if totalInput > 0 {
		parts = append(parts, formatTokenCount(totalInput)+" in")
	}
	if outputTokens > 0 {
		parts = append(parts, formatTokenCount(outputTokens)+" out")
	}
	ctxBasis := contextTokens
	if ctxBasis <= 0 {
		ctxBasis = totalInput
	}
	if ctxBasis >= 100 {
		pct := ctxBasis * 100 / modelContextWindow
		if pct > 100 {
			pct = 100
		}
		parts = append(parts, fmt.Sprintf("ctx ~%d%%", pct))
	}
	if len(parts) == 0 {
		return ""
	}
	return "[" + strings.Join(parts, " · ") + "]"
}

// Finalize updates card progress state (running/completed/failed) without
// appending a new progress entry.
func (w *compactProgressWriter) Finalize(state ProgressCardState) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.stopTickerLocked()
	if !w.enabled || w.failed || w.handle == nil {
		return false
	}
	// Compact style: freeze the active line, add usage indicator, render final block.
	if w.style != progressStyleCard {
		w.freezeActiveLocked()
		final := w.buildCompactBlock("")
		if final != w.lastSent && final != "" {
			callCtx, cancel := w.withAPITimeout()
			err := w.updater.UpdateMessage(callCtx, w.handle, final)
			cancel()
			if err != nil {
				slog.Warn("progress writer: Finalize compact UpdateMessage failed", "platform", w.platform.Name(), "error", err)
			} else {
				w.lastSent = final
				w.content = final
			}
		}
		return true
	}
	if !w.usePayload {
		return false
	}
	if state == "" {
		state = ProgressCardStateCompleted
	}
	if w.state == state {
		return true
	}
	w.state = state
	w.content = BuildProgressCardPayloadV2(w.items, w.truncated, w.agentName, w.lang, w.state)
	if w.content == "" || w.content == w.lastSent {
		return w.content != ""
	}
	callCtx, cancel := w.withAPITimeout()
	err := w.updater.UpdateMessage(callCtx, w.handle, w.content)
	cancel()
	if err != nil {
		slog.Warn("progress writer: Finalize UpdateMessage failed", "platform", w.platform.Name(), "style", w.style, "error", err)
		w.failed = true
		return false
	}
	w.lastSent = w.content
	return true
}

func (w *compactProgressWriter) withAPITimeout() (context.Context, context.CancelFunc) {
	if _, hasDeadline := w.ctx.Deadline(); hasDeadline {
		return w.ctx, func() {}
	}
	return context.WithTimeout(w.ctx, compactProgressAPITimeout)
}

func renderCardProgressMarkdownFallback(entries []string, truncated bool) string {
	var b strings.Builder
	b.WriteString("⏳ **Progress**\n")
	if truncated {
		b.WriteString("_Showing latest updates only._\n")
	}
	for i, entry := range entries {
		b.WriteString("\n")
		b.WriteString(strconv.Itoa(i + 1))
		b.WriteString(". ")
		b.WriteString(strings.ReplaceAll(entry, "\n", "\n   "))
	}
	return b.String()
}

// renderCompactStatus returns a friendly one-liner for compact progress.
// Returns "" when the caller should fall through to default behavior.
func renderCompactStatus(item ProgressCardEntry, lang Language) string {
	var s string
	switch item.Kind {
	case ProgressEntryThinking:
		s = translateMsg(lang, MsgCompactThinking)
	case ProgressEntryToolUse:
		s = renderCompactToolUse(item.Tool, item.Text, lang)
	case ProgressEntryToolResult:
		return "" // caller skips update
	default:
		return "" // fall through to fallback
	}
	// Enforce fixed max width so the message never wraps.
	if utf8.RuneCountInString(s) > compactStatusMaxLen {
		rs := []rune(s)
		s = string(rs[:compactStatusMaxLen]) + "…"
	}
	return s
}

func renderCompactToolUse(tool, input string, lang Language) string {
	// Pick the i18n template for this tool, then budget the input length
	// so the final string fits within compactStatusMaxLen.
	var key MsgKey
	switch tool {
	case "Read":
		key = MsgCompactReading
	case "Edit":
		key = MsgCompactEditing
	case "Write":
		key = MsgCompactWriting
	case "Bash", "shell", "run_shell_command":
		key = MsgCompactRunning
	case "Grep":
		key = MsgCompactSearching
	case "Glob":
		key = MsgCompactFindingFiles
	case "Agent":
		return translateMsg(lang, MsgCompactDelegating)
	default:
		key = MsgCompactToolGeneric
		if input == "" {
			input = tool
		} else {
			input = tool + ": " + input
		}
	}

	tmpl := translateMsg(lang, key)
	// Estimate how many runes the prefix takes (template minus the %s placeholder).
	prefixLen := utf8.RuneCountInString(tmpl) - 2 // "%s" = 2 chars
	budget := compactStatusMaxLen - prefixLen
	if budget < 10 {
		budget = 10
	}
	brief := compactInputBrief(input, budget)
	if brief == "" {
		brief = tool
	}
	return fmt.Sprintf(tmpl, brief)
}

// stripWorkDirPrefix replaces an absolute workDir prefix in s with a relative
// path. For example "/Users/x/project/src/main.go" → "src/main.go" when
// workDir is "/Users/x/project".
func stripWorkDirPrefix(s, workDir string) string {
	prefix := workDir + "/"
	return strings.ReplaceAll(s, prefix, "")
}

// compactInputBrief extracts a short one-line summary from tool input.
func compactInputBrief(input string, maxLen int) string {
	input = strings.TrimSpace(input)
	if input == "" {
		return ""
	}
	// Take first line only
	if i := strings.IndexByte(input, '\n'); i >= 0 {
		input = input[:i]
	}
	input = strings.TrimSpace(input)
	if maxLen > 0 && utf8.RuneCountInString(input) > maxLen {
		rs := []rune(input)
		input = string(rs[:maxLen]) + "…"
	}
	return input
}

// translateMsg looks up a translated message by language with fallback to English.
func translateMsg(lang Language, key MsgKey) string {
	if msg, ok := messages[key]; ok {
		if translated, ok := msg[lang]; ok {
			return translated
		}
		if lang == LangTraditionalChinese {
			if translated, ok := msg[LangChinese]; ok {
				return translated
			}
		}
		if msg[LangEnglish] != "" {
			return msg[LangEnglish]
		}
	}
	return string(key)
}

// freezeActiveLocked freezes the current active line (with its final elapsed
// time) into frozenLines. Must be called while w.mu is held.
func (w *compactProgressWriter) freezeActiveLocked() {
	if w.baseContent == "" {
		return
	}
	frozen := w.baseContent
	if !w.toolStartAt.IsZero() {
		frozen = w.renderActiveWithElapsed(time.Since(w.toolStartAt))
	}
	w.frozenLines = append(w.frozenLines, frozen)
	// Cap frozen lines to maxEntries.
	if w.maxEntries > 0 && len(w.frozenLines) > w.maxEntries {
		w.frozenLines = w.frozenLines[len(w.frozenLines)-w.maxEntries:]
	}
	w.toolStartAt = time.Time{}
	w.toolBaseStatus = ""
	w.activeKind = ""
	w.baseContent = ""
}

// renderActiveWithElapsed returns the active line with an elapsed-time suffix.
// For thinking events: "💭 Thinking for 5s"; for tools: "📖 Reading xyz · 5s".
func (w *compactProgressWriter) renderActiveWithElapsed(elapsed time.Duration) string {
	es := formatElapsed(elapsed)
	if w.activeKind == ProgressEntryThinking {
		return fmt.Sprintf(translateMsg(w.lang, MsgCompactThinkingElapsed), es)
	}
	return w.toolBaseStatus + " · " + es
}

// buildCompactBlock assembles all frozen lines + the active line (if any) +
// usage suffix into a single code block for Slack rendering.
func (w *compactProgressWriter) buildCompactBlock(activeLine string) string {
	var lines []string
	if w.injectHeader != "" {
		lines = append(lines, "📌 "+w.injectHeader)
	}
	lines = append(lines, w.frozenLines...)
	if activeLine != "" {
		lines = append(lines, activeLine)
	}
	if w.usageSuffix != "" {
		lines = append(lines, w.usageSuffix)
	}
	if len(lines) == 0 {
		return ""
	}
	body := strings.Join(lines, "\n")
	return "```\n" + body + "\n```"
}

// formatElapsed returns a short human-readable duration like "5s" or "2m13s".
func formatElapsed(d time.Duration) string {
	secs := int(d.Seconds())
	if secs < 60 {
		return fmt.Sprintf("%ds", secs)
	}
	return fmt.Sprintf("%dm%ds", secs/60, secs%60)
}

// stopTickerLocked stops the background elapsed-time ticker. Must be called
// while w.mu is held.
func (w *compactProgressWriter) stopTickerLocked() {
	if w.tickerStop != nil {
		close(w.tickerStop)
		w.tickerStop = nil
	}
}

// startToolTickerLocked spawns a goroutine that periodically refreshes the
// compact status line with an elapsed-time suffix. Must be called while w.mu
// is held; the goroutine acquires the lock on each tick.
func (w *compactProgressWriter) startToolTickerLocked() {
	w.tickerStop = make(chan struct{})
	stop := w.tickerStop
	go func() {
		ticker := time.NewTicker(compactElapsedTickInterval)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-w.ctx.Done():
				return
			case <-ticker.C:
				w.refreshToolElapsed()
			}
		}
	}()
}

// refreshToolElapsed is called by the ticker goroutine to update the compact
// status line with the current elapsed time and conditionally show the stop hint.
func (w *compactProgressWriter) refreshToolElapsed() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.toolBaseStatus == "" || w.failed || w.handle == nil {
		return
	}
	elapsed := time.Since(w.toolStartAt)
	activeLine := w.renderActiveWithElapsed(elapsed)
	if elapsed >= compactStopHintDelay {
		if hint := translateMsg(w.lang, MsgCompactStopHint); hint != "" {
			activeLine += " · " + hint
		}
	}
	w.content = w.buildCompactBlock(activeLine)
	if w.content == w.lastSent {
		return
	}
	if w.inCooldown() {
		return
	}
	callCtx, cancel := w.withAPITimeout()
	err := w.updater.UpdateMessage(callCtx, w.handle, w.content)
	cancel()
	if err != nil {
		if isTransientUpdateErr(err) {
			slog.Warn("progress writer: elapsed UpdateMessage transient failure; backing off", "platform", w.platform.Name(), "error", err, "cooldown", compactProgressCooldown)
			w.noteTransientErrLocked()
			return
		}
		slog.Warn("progress writer: elapsed UpdateMessage failed", "platform", w.platform.Name(), "error", err)
		w.failed = true
		return
	}
	w.cooldownUntil = time.Time{}
	w.lastSent = w.content
}

func trimCompactProgressText(s string, maxRunes int) string {
	if maxRunes <= 0 {
		return s
	}
	s = strings.TrimPrefix(s, "…\n")
	if utf8.RuneCountInString(s) <= maxRunes {
		return s
	}
	rs := []rune(s)
	tail := strings.TrimLeft(string(rs[len(rs)-maxRunes:]), "\n")
	return "…\n" + tail
}
