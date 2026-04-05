# CC-Connect Security Review Report

**Date:** 2026-04-05
**Scope:** Full codebase — `core/`, `agent/`, `platform/`, `config/`, `cmd/`, `daemon/`

---

## Executive Summary

This report covers a comprehensive security review of the cc-connect codebase. **3 critical**, **5 high**, **12 medium**, and **5 low** severity issues were identified. The most urgent findings involve a WebSocket CSRF bypass, shell command injection via webhook/cron APIs, and SSRF vulnerabilities in platform file download handlers.

The codebase demonstrates several good security practices (constant-time token comparison, structured logging, `crypto/rand` usage, proper session locking), but has significant gaps in input validation, authentication enforcement, and network-level protections.

---

## Findings Summary

| # | Severity | Category | Component | Description |
|---|----------|----------|-----------|-------------|
| 1 | CRITICAL | CSRF | `core/bridge.go` | WebSocket accepts all origins |
| 2 | CRITICAL | Injection | `core/webhook.go`, `core/engine.go` | Shell command injection via `sh -c` |
| 3 | CRITICAL | SSRF | `platform/discord`, `platform/qq`, `platform/slack`, `platform/dingtalk` | Unvalidated URL downloads |
| 4 | HIGH | AuthN | `core/webhook.go`, `core/management.go`, `core/bridge.go` | Authentication disabled when token is empty |
| 5 | HIGH | AuthN | `core/webhook.go`, `core/management.go` | Tokens accepted via URL query parameters |
| 6 | HIGH | Injection | `agent/cursor/cursor.go` | SQL injection in sqlite3 queries |
| 7 | HIGH | Path Traversal | `core/webhook.go` | Unrestricted WorkDir in webhook exec |
| 8 | HIGH | Path Traversal | `agent/pi/session.go` | Missing `filepath.Base()` on uploaded filenames |
| 9 | MEDIUM | TLS | `core/webhook.go`, `core/management.go`, `core/bridge.go` | No TLS/HTTPS support for HTTP servers |
| 10 | MEDIUM | Input Validation | `core/api.go`, `core/webhook.go` | Missing request body size limits |
| 11 | MEDIUM | CORS | `core/management.go` | Wildcard CORS echoes back any origin |
| 12 | MEDIUM | Secrets | `platform/wecom/wecom.go` | Access tokens embedded in URLs |
| 13 | MEDIUM | Config Injection | `config/config.go` | TOML injection via unsanitized credential writes |
| 14 | MEDIUM | Permissions | `daemon/manager.go`, `daemon/systemd.go` | Overly permissive file permissions (0644) |
| 15 | MEDIUM | DoS | Multiple agents | Unbounded stderr capture from subprocesses |
| 16 | MEDIUM | Info Disclosure | `agent/acp/session.go` | Subprocess stderr written directly to os.Stderr |
| 17 | MEDIUM | Rate Limiting | `core/webhook.go` | No rate limiting on webhook endpoint |
| 18 | MEDIUM | Permissions | `config/config.go` | Data directory created with 0755 |
| 19 | MEDIUM | AuthZ | `config/config.go` | `admin_from = "*"` allows all users privileged commands |
| 20 | MEDIUM | Temp Files | `agent/gemini/session.go` | Silent fallback to system temp dir on mkdir failure |
| 21 | LOW | Redaction | `core/redact.go` | Incomplete sensitive flag list in RedactArgs |
| 22 | LOW | Panic | `core/cron.go` | `panic()` on `rand.Read` failure in cron ID generation |
| 23 | LOW | Race | `core/bridge.go` | WebSocket connection replacement while old conn in use |
| 24 | LOW | Path Traversal | `agent/cursor/cursor.go` | Unvalidated sessionID in database path construction |
| 25 | LOW | Audit | Codebase-wide | No audit logging for sensitive operations |

---

## Critical Findings

### 1. WebSocket CSRF — All Origins Accepted

**File:** `core/bridge.go:124-126`

```go
var wsUpgrader = websocket.Upgrader{
    CheckOrigin: func(r *http.Request) bool { return true },
}
```

**Impact:** Any malicious website can establish a WebSocket connection to the bridge server and send/receive messages on behalf of a connected platform adapter. This enables Cross-Site WebSocket Hijacking (CSWSH).

**Recommendation:** Validate the `Origin` header against a configurable allowlist. At minimum, restrict to localhost when not explicitly configured.

---

### 2. Shell Command Injection via Webhook & Cron

**Files:** `core/webhook.go:308`, `core/engine.go:960,3557,8081`

```go
cmd := exec.CommandContext(ctx, "sh", "-c", req.Exec)   // webhook
cmd := exec.CommandContext(ctx, "sh", "-c", job.Exec)    // cron
```

**Impact:** User-supplied `Exec` strings from authenticated API requests are passed directly to `sh -c`. An authenticated attacker (or anyone if tokens are unconfigured — see Finding #4) can execute arbitrary system commands.

**Recommendation:**
- Use `exec.Command()` with an argument array instead of shell strings.
- If shell features are required, implement strict command validation or a restricted allowlist.
- Never pass untrusted input to `sh -c`.

---

### 3. SSRF via Unvalidated File Downloads

**Files:**
- `platform/discord/discord.go:1208-1218`
- `platform/qq/qq.go:530-548`
- `platform/slack/slack.go:417-446`
- `platform/dingtalk/dingtalk.go:264-294`

```go
// discord example
func downloadURL(u string) ([]byte, error) {
    resp, err := core.HTTPClient.Get(u) // No URL validation
```

**Impact:** Attachment URLs from incoming messages are fetched without domain validation. An attacker can craft messages with internal network URLs (`http://169.254.169.254/...`, `http://localhost:8080/...`) to perform Server-Side Request Forgery.

**Recommendation:** Implement per-platform URL allowlists (e.g., Discord downloads only from `cdn.discordapp.com`). Block private/internal IP ranges and link-local addresses.

---

## High Findings

### 4. Optional Authentication on HTTP APIs

**Files:** `core/webhook.go:155-179`, `core/management.go:229-240`, `core/bridge.go`

```go
func (bs *BridgeServer) authenticate(r *http.Request) bool {
    if bs.token == "" {
        return true // NO AUTHENTICATION
    }
```

**Impact:** When token configuration is empty (the default), all HTTP APIs are completely open. The bridge WebSocket has no token-based authentication at all.

**Recommendation:** Require tokens for all APIs in production. Generate secure random defaults if none provided. Log a prominent warning when running without authentication.

---

### 5. Token Authentication via Query Parameters

**Files:** `core/webhook.go:173-176`, `core/management.go:238-239`

```go
if tok := r.URL.Query().Get("token"); tok != "" {
    return subtle.ConstantTimeCompare([]byte(tok), []byte(ws.token)) == 1
}
```

**Impact:** Tokens in URL query strings are recorded in server access logs, browser history, proxy logs, and HTTP Referer headers.

**Recommendation:** Accept tokens only via `Authorization: Bearer` or custom headers. Remove query parameter support.

---

### 6. SQL Injection in Cursor Agent

**File:** `agent/cursor/cursor.go:467,503-506`

```go
fmt.Sprintf("SELECT hex(substr(data,1,8192)) FROM blobs WHERE id='%s' LIMIT 1;", rootBlobID)
```

**Impact:** Values interpolated into SQL strings passed to `sqlite3` CLI. While inputs originate from internal database data, a corrupted or attacker-controlled database could inject arbitrary SQL.

**Recommendation:** Use parameterized queries via Go's `database/sql` package, or strictly validate that IDs contain only hex characters.

---

### 7. Unrestricted WorkDir in Webhook Exec

**File:** `core/webhook.go:295-303`

```go
workDir := req.WorkDir
// ...
cmd.Dir = workDir
```

**Impact:** The webhook API allows callers to specify an arbitrary working directory for command execution with no path validation.

**Recommendation:** Validate `workDir` against allowed base directories. Resolve symlinks and reject paths containing `..`.

---

### 8. Path Traversal in Pi Agent File Upload

**File:** `agent/pi/session.go:442-446`

```go
fname := img.FileName       // NOT sanitized with filepath.Base()
fpath := filepath.Join(attachDir, fname)
```

**Impact:** Unlike the Gemini agent (which correctly uses `filepath.Base()`), the Pi agent uses raw filenames. A filename like `../../etc/cron.d/backdoor` writes outside the attachment directory.

**Recommendation:** Apply `filepath.Base()` to sanitize the filename, matching the pattern used in `agent/gemini/session.go`.

---

## Medium Findings

### 9. No TLS for HTTP Servers

All three HTTP servers (`webhook`, `management`, `bridge`) use `ListenAndServe()` without TLS. If bound to a non-localhost interface, all traffic including authentication tokens is transmitted in plaintext.

**Recommendation:** Add `ListenAndServeTLS()` support with configurable cert/key paths.

### 10. Missing Request Body Size Limits

`core/api.go` uses `io.LimitReader` for `/send` (52 MB), but `/cron/add` and `/webhook` have no body size limits. Unbounded JSON decoders enable memory exhaustion DoS.

**Recommendation:** Apply `io.LimitReader` to all request body decoders.

### 11. Wildcard CORS Echoes Origin

`core/management.go:244-258` — When CORS is configured with `"*"`, the server echoes back the request's `Origin` header. Combined with credentials, this defeats CORS protections entirely.

**Recommendation:** Never echo arbitrary origins when credentials are allowed.

### 12. Access Tokens in URLs (WeChat Work)

`platform/wecom/wecom.go` constructs API URLs with access tokens as query parameters (`?access_token=...`), which appear in logs and proxy caches.

**Recommendation:** Use HTTP headers for token transport where the API supports it.

### 13. TOML Config Injection

`config/config.go:1021-1147` — Credential values are written to TOML files via string operations without proper escaping. Values containing quotes or newlines could corrupt config structure.

**Recommendation:** Use proper TOML marshaling instead of string concatenation.

### 14. Overly Permissive Daemon File Permissions

`daemon/manager.go`, `daemon/systemd.go`, `daemon/launchd.go` create files with `0644`. Config and log files may contain sensitive data readable by other system users.

**Recommendation:** Use `0600` for config/metadata files, `0700` for directories.

### 15. Unbounded Stderr Capture from Subprocesses

Multiple agents (`cursor/session.go`, `codex/session.go`, etc.) capture subprocess stderr into unbounded `bytes.Buffer`. A malicious or buggy subprocess can exhaust memory.

**Recommendation:** Use a size-limited buffer or `io.LimitedWriter`.

### 16. Subprocess Stderr Leak (ACP Agent)

`agent/acp/session.go:86` writes subprocess stderr directly to `os.Stderr`, potentially exposing secrets from child processes.

**Recommendation:** Buffer stderr and redact sensitive patterns before logging.

### 17-20. Rate Limiting, Directory Permissions, Admin Wildcard, Temp File Fallback

See summary table above for details. These are standard hardening measures.

---

## Positive Findings

The codebase demonstrates several strong security practices:

- **Constant-time token comparison** using `subtle.ConstantTimeCompare` across all auth checks
- **Structured logging** via `slog` — better than unstructured `log.Printf`
- **`crypto/rand`** used for nonce and ID generation (not `math/rand`)
- **Proper session locking** with `sync.Mutex` and `sync.Once` for teardown
- **Feishu sanitizing logger** (`platform/feishu/feishu.go`) that masks sensitive query parameters — good pattern to replicate
- **No hardcoded credentials** in source code
- **Agent command injection prevention** — all agents use `exec.CommandContext()` with argument arrays for subprocess spawning
- **Path traversal protection** in Gemini agent with `filepath.Base()` and `filepath.EvalSymlinks()`
- **Current dependencies** — no known vulnerable versions detected in `go.mod`
- **Message deduplication** in several platforms prevents replay attacks

---

## Remediation Priority

### Immediate (This Sprint)

1. Fix WebSocket `CheckOrigin` to validate against allowlist
2. Remove `sh -c` command injection vectors — use argument arrays
3. Add URL validation/allowlists for platform file downloads
4. Make API token authentication mandatory (or warn prominently)

### Short-Term (Next Sprint)

5. Remove query parameter token support
6. Add path validation for webhook `WorkDir` and Pi agent filenames
7. Fix SQL string interpolation in cursor agent
8. Add `io.LimitReader` to all JSON decoders
9. Add TLS support for HTTP servers

### Medium-Term

10. Harden file permissions (daemon, data directory)
11. Implement rate limiting on webhook and bridge endpoints
12. Add audit logging for sensitive operations
13. Apply Feishu's sanitizing logger pattern to all platforms
14. Bound subprocess stderr capture buffers
