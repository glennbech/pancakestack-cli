package seestar

import (
	"bufio"
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha1"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
)

// CommandPort is the TCP port the scope exposes its JSON-RPC command
// socket on. Messages are `\r\n`-terminated JSON objects.
const CommandPort = 4700

// rpcReadTimeout bounds any single read. A silent socket for this
// long is treated as a wedged scope — reconnect on the next call.
const rpcReadTimeout = 15 * time.Second

// Client owns one persistent authenticated JSON-RPC connection.
// Safe for one caller at a time — the sync loop is single-goroutine
// so we don't bother with a request pipeline.
type Client struct {
	ip      string
	keyPath string

	mu     sync.Mutex
	conn   net.Conn
	rd     *bufio.Reader
	nextID int64
}

// NewClient returns an unconnected client for the given scope IP. The
// TCP dial + RSA handshake are deferred until the first Call — so a
// caller that only needs Discover doesn't pay for auth.
//
// keyPath is the RSA PEM private key extracted from ZWO's Android app.
// Empty string is allowed — auth is skipped, which works on pre-7.18
// firmware. Set via env `SEESTAR_KEY_PATH` or `~/.seestarpy/seestar.pem`
// (mirrors seestarpy's discovery order so a user with an existing setup
// works out of the box).
func NewClient(ip, keyPath string) *Client {
	return &Client{ip: ip, keyPath: keyPath}
}

// DefaultKeyPath returns the first RSA key path found on this host.
// Search order:
//  1. $SEESTAR_KEY_PATH (explicit override, e.g. CI runners)
//  2. ~/.config/pancakestack/seestar.pem (this CLI's config dir — primary)
//  3. ./seestar.pem (cwd, for one-off scripts)
//  4. ~/.seestarpy/seestar.pem (seestarpy compat — users who already
//     used that tool don't have to move the file)
//
// Returns "" if nothing found — caller then decides whether to warn.
func DefaultKeyPath() string {
	if p := os.Getenv("SEESTAR_KEY_PATH"); p != "" {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	if p := configSeestarKeyPath(); p != "" {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	if _, err := os.Stat("seestar.pem"); err == nil {
		return "seestar.pem"
	}
	if home, err := os.UserHomeDir(); err == nil {
		p := filepath.Join(home, ".seestarpy", "seestar.pem")
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

// configSeestarKeyPath resolves the CLI config dir's key path.
// Returns "" only when UserHomeDir fails — very unusual and best
// handled by falling through to the other lookups.
func configSeestarKeyPath() string {
	if v := os.Getenv("XDG_CONFIG_HOME"); v != "" {
		return filepath.Join(v, "pancakestack", "seestar.pem")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".config", "pancakestack", "seestar.pem")
}

// Close drops the socket. Safe to call multiple times.
func (c *Client) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn != nil {
		err := c.conn.Close()
		c.conn = nil
		c.rd = nil
		return err
	}
	return nil
}

// ensureConnected opens the TCP socket and runs the auth handshake if
// this is the first call (or the socket was dropped). Caller must hold
// c.mu.
func (c *Client) ensureConnected(ctx context.Context) error {
	if c.conn != nil {
		return nil
	}
	d := net.Dialer{Timeout: 5 * time.Second}
	conn, err := d.DialContext(ctx, "tcp", net.JoinHostPort(c.ip, fmt.Sprint(CommandPort)))
	if err != nil {
		return fmt.Errorf("dial %s:%d: %w", c.ip, CommandPort, err)
	}
	c.conn = conn
	c.rd = bufio.NewReaderSize(conn, 64<<10)
	if err := c.handshake(ctx); err != nil {
		_ = c.conn.Close()
		c.conn = nil
		c.rd = nil
		return err
	}
	return nil
}

// rpcFrame is the union shape of every JSON-RPC reply seen on the
// wire. Unknown fields are tolerated (the scope sends more).
type rpcFrame struct {
	ID     int64           `json:"id"`
	Code   *int            `json:"code,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Method string          `json:"method,omitempty"`
	Event  string          `json:"Event,omitempty"`
	Error  json.RawMessage `json:"error,omitempty"`
}

// Call sends one JSON-RPC request and returns the raw `result` field
// of the reply whose id matches. Silently skips interleaved event
// frames (`PiStatus`, `temp`, `Stack`, …) that the scope emits on
// the same socket while a session is running.
//
// One retry on ConnectionReset / EOF — the scope drops idle sockets
// after a few minutes.
func (c *Client) Call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		if err := c.ensureConnected(ctx); err != nil {
			lastErr = err
			continue
		}
		id := atomic.AddInt64(&c.nextID, 1) + 100 // start well past handshake ids
		req := map[string]any{
			"id":     id,
			"verify": true,
			"method": method,
		}
		if params != nil {
			req["params"] = params
		}
		result, err := c.roundtrip(ctx, id, req)
		if err == nil {
			return result, nil
		}
		lastErr = err
		// Retry only on transport errors; a JSON code>0 from the scope
		// is a real reply and we should surface it.
		if !isTransportErr(err) {
			return nil, err
		}
		_ = c.conn.Close()
		c.conn = nil
		c.rd = nil
	}
	return nil, lastErr
}

func (c *Client) roundtrip(ctx context.Context, id int64, req map[string]any) (json.RawMessage, error) {
	payload, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	payload = append(payload, '\r', '\n')
	if dl, ok := ctx.Deadline(); ok {
		_ = c.conn.SetWriteDeadline(dl)
	} else {
		_ = c.conn.SetWriteDeadline(time.Now().Add(rpcReadTimeout))
	}
	if _, err := c.conn.Write(payload); err != nil {
		return nil, err
	}
	// Read reply frames until we see one with matching id.
	for {
		if dl, ok := ctx.Deadline(); ok {
			_ = c.conn.SetReadDeadline(dl)
		} else {
			_ = c.conn.SetReadDeadline(time.Now().Add(rpcReadTimeout))
		}
		line, err := c.rd.ReadBytes('\n')
		if err != nil {
			return nil, err
		}
		// Strip trailing \r\n / whitespace.
		for len(line) > 0 && (line[len(line)-1] == '\n' || line[len(line)-1] == '\r' || line[len(line)-1] == ' ') {
			line = line[:len(line)-1]
		}
		if len(line) == 0 {
			continue
		}
		var frame rpcFrame
		if err := json.Unmarshal(line, &frame); err != nil {
			continue
		}
		if frame.ID != id {
			continue // interleaved event / stale reply
		}
		if frame.Code != nil && *frame.Code != 0 {
			return nil, fmt.Errorf("seestar %s: code=%d err=%s", req["method"], *frame.Code, string(frame.Error))
		}
		return frame.Result, nil
	}
}

func isTransportErr(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, net.ErrClosed) {
		return true
	}
	msg := err.Error()
	// Cheap keyword match — the Go net stack doesn't export the errors
	// we care about (EOF, connection reset) as sentinels reliably.
	for _, k := range []string{"EOF", "connection reset", "broken pipe", "i/o timeout", "closed"} {
		if containsFold(msg, k) {
			return true
		}
	}
	return false
}

func containsFold(s, sub string) bool {
	// Small local helper to avoid pulling in strings for one call.
	if len(sub) > len(s) {
		return false
	}
	for i := 0; i+len(sub) <= len(s); i++ {
		match := true
		for j := 0; j < len(sub); j++ {
			a := s[i+j]
			b := sub[j]
			if a >= 'A' && a <= 'Z' {
				a += 'a' - 'A'
			}
			if b >= 'A' && b <= 'Z' {
				b += 'a' - 'A'
			}
			if a != b {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

// --- Handshake ---------------------------------------------------------

// handshake runs the firmware 7.18+ RSA challenge-response. If no key
// is available (`c.keyPath` empty or file missing) we skip it — the
// scope will reject listing calls with `code=103` and Call bubbles
// that up with a readable message.
//
// Caller must hold c.mu and have c.conn/c.rd populated.
func (c *Client) handshake(ctx context.Context) error {
	// Ask for a challenge. Some firmwares don't implement this method
	// (`code=103`), and pre-7.18 firmware returns an empty string —
	// treat both as "no auth needed".
	result, err := c.rawCall(ctx, 1001, "get_verify_str", "verify")
	if err != nil {
		// A code=103 comes back as an error via rawCall; treat any
		// handshake error as skip-and-hope for older firmwares.
		return nil
	}
	var chall struct {
		Str string `json:"str"`
	}
	// The scope sometimes returns the challenge as a bare string, sometimes
	// as `{"str":"..."}`. Handle both.
	if len(result) > 0 && result[0] == '"' {
		var s string
		if json.Unmarshal(result, &s) == nil {
			chall.Str = s
		}
	} else {
		_ = json.Unmarshal(result, &chall)
	}
	if chall.Str == "" {
		return nil
	}
	if c.keyPath == "" {
		return fmt.Errorf("scope requires RSA auth but no key available — set SEESTAR_KEY_PATH or drop the PEM at ~/.seestarpy/seestar.pem (extract from ZWO app; see astronomyk/seestarpy)")
	}
	sig, err := signChallenge(c.keyPath, chall.Str)
	if err != nil {
		return fmt.Errorf("sign challenge: %w", err)
	}
	if _, err := c.rawCall(ctx, 1002, "verify_client", map[string]any{
		"sign": sig,
		"data": chall.Str,
	}); err != nil {
		return fmt.Errorf("verify_client rejected — the PEM at %s doesn't match this firmware", c.keyPath)
	}
	// Best-effort confirm; non-fatal.
	_, _ = c.rawCall(ctx, 1003, "pi_is_verified", "verify")
	return nil
}

// rawCall is the pre-auth version of Call — used inside handshake() so
// we don't recurse through ensureConnected. Assumes caller holds c.mu
// and c.conn is live.
func (c *Client) rawCall(ctx context.Context, id int64, method string, params any) (json.RawMessage, error) {
	req := map[string]any{
		"id":     id,
		"verify": true,
		"method": method,
	}
	if params != nil {
		req["params"] = params
	}
	return c.roundtrip(ctx, id, req)
}

// signChallenge signs a challenge string with RSA-PKCS1v15-SHA1 and
// returns the base64-encoded signature — the shape the scope expects.
func signChallenge(keyPath, challenge string) (string, error) {
	raw, err := os.ReadFile(keyPath)
	if err != nil {
		return "", err
	}
	block, _ := pem.Decode(raw)
	if block == nil {
		return "", errors.New("no PEM block found")
	}
	var key *rsa.PrivateKey
	switch block.Type {
	case "RSA PRIVATE KEY":
		key, err = x509.ParsePKCS1PrivateKey(block.Bytes)
	case "PRIVATE KEY":
		k, perr := x509.ParsePKCS8PrivateKey(block.Bytes)
		if perr != nil {
			return "", perr
		}
		var ok bool
		key, ok = k.(*rsa.PrivateKey)
		if !ok {
			return "", errors.New("PEM is not an RSA key")
		}
	default:
		return "", fmt.Errorf("unsupported PEM type %q", block.Type)
	}
	if err != nil {
		return "", err
	}
	h := sha1.Sum([]byte(challenge))
	sig, err := rsa.SignPKCS1v15(nil, key, crypto.SHA1, h[:])
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(sig), nil
}
