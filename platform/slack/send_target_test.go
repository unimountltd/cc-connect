package slack

import "testing"

func TestOverrideReplyTarget(t *testing.T) {
	p := &Platform{}
	cur := replyContext{channel: "C0PRODUCT", timestamp: "1792660000.000100", messageTS: "1792660005.000200"}

	got, err := p.OverrideReplyTarget(cur, "", "1792670000.000900", false)
	if err != nil {
		t.Fatalf("thread: %v", err)
	}
	if rc := got.(replyContext); rc.channel != "C0PRODUCT" || rc.timestamp != "1792670000.000900" {
		t.Fatalf("thread override = %+v", rc)
	}

	got, err = p.OverrideReplyTarget(cur, "", "", true)
	if err != nil {
		t.Fatalf("root: %v", err)
	}
	if rc := got.(replyContext); rc.channel != "C0PRODUCT" || rc.timestamp != "" {
		t.Fatalf("root override = %+v", rc)
	}

	got, err = p.OverrideReplyTarget(cur, "C0OPS1234", "1792680000.000001", false)
	if err != nil {
		t.Fatalf("channel: %v", err)
	}
	if rc := got.(replyContext); rc.channel != "C0OPS1234" || rc.timestamp != "1792680000.000001" || rc.messageTS != "" {
		t.Fatalf("channel override = %+v (messageTS must be cleared for another channel)", rc)
	}

	// The original context is not modified.
	if cur.timestamp != "1792660000.000100" {
		t.Fatalf("original ctx mutated: %+v", cur)
	}

	// No cached context (e.g. reconstructed nil) needs a channel.
	if _, err := p.OverrideReplyTarget(nil, "", "1792670000.000900", false); err == nil {
		t.Fatal("expected error without a channel")
	}
	for _, bad := range []string{"abc", "1792670000", "1792670000.000900 x", "<#C1>"} {
		if _, err := p.OverrideReplyTarget(cur, "", bad, false); err == nil {
			t.Errorf("thread %q should be rejected", bad)
		}
	}
	if _, err := p.OverrideReplyTarget(cur, "#product", "", true); err == nil {
		t.Error("channel name instead of id should be rejected")
	}
}

func TestDescribeReplyTarget(t *testing.T) {
	p := &Platform{}
	ch, th, ok := p.DescribeReplyTarget(replyContext{channel: "C1", timestamp: "1.2"})
	if !ok || ch != "C1" || th != "1.2" {
		t.Fatalf("describe = %q %q %v", ch, th, ok)
	}
	if _, _, ok := p.DescribeReplyTarget("not-a-ctx"); ok {
		t.Fatal("foreign ctx should not describe")
	}
}
