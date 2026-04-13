//go:build darwin

package daemon

import (
	"strings"
	"testing"
)

func TestBuildPlist_KeepAliveUnconditional(t *testing.T) {
	cfg := Config{
		BinaryPath: "/opt/cc-connect/cc-connect",
		WorkDir:    "/tmp/wd",
		LogFile:    "/tmp/log",
		LogMaxSize: 10485760,
		EnvPATH:    "/usr/bin",
	}
	xml := buildPlist(cfg)
	// Unconditional KeepAlive ensures launchd restarts the process after any
	// exit, including SIGKILL / OOM kills that leave stale state with
	// conditional SuccessfulExit policies.
	if !strings.Contains(xml, "<key>KeepAlive</key>\n\t<true/>") {
		t.Fatal("plist must use unconditional KeepAlive true")
	}
	if strings.Contains(xml, "<key>SuccessfulExit</key>") {
		t.Fatal("plist must not use conditional SuccessfulExit — it fails to restart after SIGKILL")
	}
}
