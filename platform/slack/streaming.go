package slack

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/chenhg5/cc-connect/core"

	"github.com/slack-go/slack"
)

// slackPreviewHandle points at the in-flight streaming-preview message so
// UpdateMessage can edit it in place via chat.update.
type slackPreviewHandle struct {
	channel   string
	timestamp string
}

// SendPreviewStart posts the initial streaming-preview message (threaded like a
// normal reply) and returns a handle for subsequent edits. Implements
// core.PreviewStarter; together with UpdateMessage it lights up the engine's
// real-time streaming preview for Slack (the engine throttles the edits, so we
// stay within chat.update rate limits).
func (p *Platform) SendPreviewStart(ctx context.Context, rctx any, content string) (any, error) {
	rc, ok := rctx.(replyContext)
	if !ok {
		return nil, fmt.Errorf("slack: invalid reply context type %T", rctx)
	}
	opts := []slack.MsgOption{
		slack.MsgOptionText(core.MarkdownToSlackMrkdwn(content), false),
	}
	if rc.timestamp != "" {
		opts = append(opts, slack.MsgOptionPostMessageParameters(slack.PostMessageParameters{ThreadTimestamp: rc.timestamp}))
	}
	channel, ts, err := p.client.PostMessageContext(ctx, rc.channel, opts...)
	if err != nil {
		return nil, fmt.Errorf("slack: send preview: %w", err)
	}
	return &slackPreviewHandle{channel: channel, timestamp: ts}, nil
}

// UpdateMessage edits the preview message in place. The engine passes the handle
// returned by SendPreviewStart (not the reply context). Implements
// core.MessageUpdater.
func (p *Platform) UpdateMessage(ctx context.Context, previewHandle any, content string) error {
	h, ok := previewHandle.(*slackPreviewHandle)
	if !ok {
		return fmt.Errorf("slack: invalid preview handle type %T", previewHandle)
	}
	_, _, _, err := p.client.UpdateMessageContext(ctx, h.channel, h.timestamp,
		slack.MsgOptionText(core.MarkdownToSlackMrkdwn(content), false),
	)
	if err == nil {
		return nil
	}
	if strings.Contains(err.Error(), "message_not_found") {
		return nil
	}

	// Slack tier-3 rate limits can be hit during long turns. Retry once when
	// Slack supplies a delay that fits inside the caller's context deadline.
	var rateLimitErr *slack.RateLimitedError
	if errors.As(err, &rateLimitErr) && rateLimitErr.RetryAfter > 0 &&
		p.waitWithinDeadline(ctx, rateLimitErr.RetryAfter) {
		_, _, _, retryErr := p.client.UpdateMessageContext(ctx, h.channel, h.timestamp,
			slack.MsgOptionText(core.MarkdownToSlackMrkdwn(content), false),
		)
		if retryErr == nil || strings.Contains(retryErr.Error(), "message_not_found") {
			return nil
		}
		return fmt.Errorf("slack: update preview after retry: %w", retryErr)
	}
	return fmt.Errorf("slack: update preview: %w", err)
}

// waitWithinDeadline waits only when the remaining context budget can cover
// both Slack's requested delay and a small buffer for the retry itself.
func (p *Platform) waitWithinDeadline(ctx context.Context, delay time.Duration) bool {
	if delay <= 0 {
		return true
	}
	if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) < delay+500*time.Millisecond {
		return false
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}

func (p *Platform) DeletePreviewMessage(ctx context.Context, previewHandle any) error {
	h, ok := previewHandle.(*slackPreviewHandle)
	if !ok {
		return fmt.Errorf("slack: invalid preview handle type %T", previewHandle)
	}
	_, _, err := p.client.DeleteMessageContext(ctx, h.channel, h.timestamp)
	if err != nil && !strings.Contains(err.Error(), "message_not_found") {
		return fmt.Errorf("slack: delete preview: %w", err)
	}
	return nil
}

func (p *Platform) ProgressStyle() string {
	return "compact"
}

var (
	_ core.MessageUpdater        = (*Platform)(nil)
	_ core.PreviewStarter        = (*Platform)(nil)
	_ core.PreviewCleaner        = (*Platform)(nil)
	_ core.ProgressStyleProvider = (*Platform)(nil)
)
