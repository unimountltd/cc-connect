package main

import "testing"

func TestParseSendArgs_ThreadRootChannelDryRun(t *testing.T) {
	t.Setenv("CC_SESSION_KEY", "slack:C0PRODUCT")

	req, _, err := parseSendArgs([]string{"--thread", "1792670000.000900", "-m", "hi"})
	if err != nil || req.Thread != "1792670000.000900" || req.Root || req.Channel != "" {
		t.Fatalf("--thread: req=%+v err=%v", req, err)
	}
	req, _, err = parseSendArgs([]string{"-t", "1.2", "hi"})
	if err != nil || req.Thread != "1.2" || req.Message != "hi" {
		t.Fatalf("-t: req=%+v err=%v", req, err)
	}
	req, _, err = parseSendArgs([]string{"--root", "-m", "check-in"})
	if err != nil || !req.Root || req.Thread != "" {
		t.Fatalf("--root: req=%+v err=%v", req, err)
	}
	req, _, err = parseSendArgs([]string{"--channel", "C0OPS", "--thread", "1.2", "-m", "x"})
	if err != nil || req.Channel != "C0OPS" {
		t.Fatalf("--channel: req=%+v err=%v", req, err)
	}
	// Dry run needs no message.
	req, _, err = parseSendArgs([]string{"--thread", "1.2", "--dry-run"})
	if err != nil || !req.DryRun {
		t.Fatalf("--dry-run: req=%+v err=%v", req, err)
	}
	// No flags: nothing set, old behaviour.
	req, _, err = parseSendArgs([]string{"-m", "plain"})
	if err != nil || req.Thread != "" || req.Root || req.Channel != "" || req.DryRun {
		t.Fatalf("plain: req=%+v err=%v", req, err)
	}

	for _, bad := range [][]string{
		{"--thread", "1.2", "--root", "-m", "x"},
		{"--channel", "C0OPS", "-m", "x"},
		{"--thread"},
		{"--thread", " ", "-m", "x"},
		{"--root", "--session-cmd", "/new"},
	} {
		if _, _, err := parseSendArgs(bad); err == nil {
			t.Errorf("args %v should fail", bad)
		}
	}
}

func TestBuildSendPayload_CarriesTarget(t *testing.T) {
	req, _, err := parseSendArgs([]string{"--thread", "1792670000.000900", "-m", "hi"})
	if err != nil {
		t.Fatal(err)
	}
	data, err := buildSendPayload(req)
	if err != nil {
		t.Fatal(err)
	}
	var back = req
	back.Thread = ""
	if err := decodeSendPayload(data, &back); err != nil || back.Thread != "1792670000.000900" {
		t.Fatalf("round trip thread = %q err=%v (payload %s)", back.Thread, err, data)
	}
}
