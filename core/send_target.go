package core

import (
	"errors"
	"fmt"
)

// Fork: explicit thread targets for `cc-connect send`.
//
// Without a target, an API/CLI send posts with the session's reply context:
// during a turn that is the thread the session is currently answering, which
// put unrelated updates into the wrong Slack thread on channel-shared
// sessions (Kulcho, 4 Nov 2026). A SendTarget lets the caller name the
// thread (or the channel root) the post belongs to. An empty SendTarget keeps
// the previous behaviour exactly.

// SendTarget overrides where an outbound API/CLI send is posted.
type SendTarget struct {
	// Channel optionally redirects the post to another channel/chat of the
	// same platform. It must be combined with Thread or Root.
	Channel string `json:"channel,omitempty"`
	// Thread is the platform thread id (Slack: the thread's root ts).
	Thread string `json:"thread,omitempty"`
	// Root posts at the channel root instead of any thread.
	Root bool `json:"root,omitempty"`
}

// IsSet reports whether the caller asked for an explicit target.
func (t SendTarget) IsSet() bool {
	return t.Channel != "" || t.Thread != "" || t.Root
}

// Validate rejects contradictory or incomplete targets.
func (t SendTarget) Validate() error {
	if t.Thread != "" && t.Root {
		return errors.New("--thread and --root cannot be combined")
	}
	if t.Channel != "" && t.Thread == "" && !t.Root {
		return errors.New("--channel needs --thread <ts> or --root")
	}
	return nil
}

// ReplyTargetOverrider is an optional interface for platforms whose reply
// context can be pointed at an explicit channel/thread.
type ReplyTargetOverrider interface {
	// OverrideReplyTarget returns a copy of rctx aimed at the given target.
	// channel == "" keeps rctx's channel; root clears any thread.
	OverrideReplyTarget(rctx any, channel, thread string, root bool) (any, error)
}

// ReplyTargetDescriber is an optional interface for platforms that can say
// which channel and thread a reply context posts to.
type ReplyTargetDescriber interface {
	DescribeReplyTarget(rctx any) (channel, thread string, ok bool)
}

// applySendTarget returns rctx re-aimed at t. An unset target returns rctx
// unchanged.
func applySendTarget(p Platform, rctx any, t SendTarget) (any, error) {
	if !t.IsSet() {
		return rctx, nil
	}
	if err := t.Validate(); err != nil {
		return nil, err
	}
	if p == nil {
		return nil, errors.New("no platform to apply --thread/--root to")
	}
	o, ok := p.(ReplyTargetOverrider)
	if !ok {
		return nil, fmt.Errorf("platform %s: --thread/--channel/--root: %w", p.Name(), ErrNotSupported)
	}
	return o.OverrideReplyTarget(rctx, t.Channel, t.Thread, t.Root)
}

// replyThreadID returns the thread a reply context posts into, or "" when the
// platform cannot tell or the reply goes to the channel root.
func replyThreadID(p Platform, rctx any) string {
	if p == nil || rctx == nil {
		return ""
	}
	d, ok := p.(ReplyTargetDescriber)
	if !ok {
		return ""
	}
	_, thread, ok := d.DescribeReplyTarget(rctx)
	if !ok {
		return ""
	}
	return thread
}

// SendTargetPreview describes where a send would be posted (send --dry-run).
type SendTargetPreview struct {
	Platform string `json:"platform"`
	Channel  string `json:"channel"`
	Thread   string `json:"thread"`
	Root     bool   `json:"root"`
}

// PreviewSendTarget resolves the session and target exactly like a text send
// would, without posting anything.
func (e *Engine) PreviewSendTarget(sessionKey string, t SendTarget) (SendTargetPreview, error) {
	if err := t.Validate(); err != nil {
		return SendTargetPreview{}, err
	}
	_, p, replyCtx, err := e.resolveOutboundSessionTarget(sessionKey, false)
	if err != nil {
		return SendTargetPreview{}, err
	}
	if p == nil {
		return SendTargetPreview{}, fmt.Errorf("no active session found (key=%q)", sessionKey)
	}
	replyCtx, err = applySendTarget(p, replyCtx, t)
	if err != nil {
		return SendTargetPreview{}, err
	}
	out := SendTargetPreview{Platform: p.Name()}
	if d, ok := p.(ReplyTargetDescriber); ok {
		if ch, th, ok := d.DescribeReplyTarget(replyCtx); ok {
			out.Channel, out.Thread, out.Root = ch, th, th == ""
		}
	}
	return out, nil
}
