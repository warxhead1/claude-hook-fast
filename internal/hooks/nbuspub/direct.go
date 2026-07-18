// Go-native bus delivery for the detached publish worker.
//
// One `nervous publish` spawns 3-5 python interpreters (channel parse,
// jsonschema validation, redis delivery) — measured 2.5-3.9s per event
// under load, ~500ms idle. The worker replicates the exact delivery
// contract natively: CloudEvents envelope -> debug.jsonl append
// (durability backstop the redis-mirror replays) -> dedup claim ->
// XADD nbus:<ch> + nbus:all over raw RESP. ~2-5ms, zero spawns.
// The nervous CLI remains the fallback when direct delivery errors.
package nbuspub

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const debugLogMaxBytes = 524288000 // mirrors nervous _log_max

type envelope struct {
	SpecVersion     string          `json:"specversion"`
	ID              string          `json:"id"`
	Source          string          `json:"source"`
	Type            string          `json:"type"`
	Time            string          `json:"time"`
	DataContentType string          `json:"datacontenttype"`
	Data            json.RawMessage `json:"data"`
}

// PublishDirect delivers one event natively. Fail-soft like nervous:
// a debug.jsonl-only outcome (redis down) is success — the mirror
// replays it. Returns an error only when the payload is unusable or
// both durability and delivery failed, so the caller can fall back.
func PublishDirect(channel, source, payload string) error {
	if channel == "" || !json.Valid([]byte(payload)) {
		return fmt.Errorf("nbuspub direct: invalid channel/payload")
	}
	env := envelope{
		SpecVersion:     "1.0",
		ID:              ulidish(),
		Source:          source,
		Type:            channel,
		Time:            time.Now().UTC().Format("2006-01-02T15:04:05Z"),
		DataContentType: "application/json",
		Data:            json.RawMessage(payload),
	}
	raw, err := json.Marshal(env)
	if err != nil {
		return err
	}
	logged := appendDebugLog(raw)
	if os.Getenv("NERVOUS_NO_REDIS") == "1" {
		return nil
	}
	if err := redisDeliver(env, raw); err != nil {
		if logged {
			return nil // durable in debug.jsonl; mirror delivers
		}
		return err
	}
	return nil
}

// ulidish mirrors nervous's ulid(): unix-ms timestamp (sortable
// prefix) + 16 random uppercase hex chars.
func ulidish() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return fmt.Sprintf("%013d%s", time.Now().UnixMilli(),
		strings.ToUpper(hex.EncodeToString(b[:])))
}

func appendDebugLog(raw []byte) bool {
	path := os.Getenv("NERVOUS_DEBUG_LOG")
	if path == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return false
		}
		path = filepath.Join(home, ".cache", "nervous-bus", "debug.jsonl")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return false
	}
	if st, err := os.Stat(path); err == nil && st.Size() > debugLogMaxBytes {
		_ = os.Rename(path, path+".1")
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	_, err = f.Write(append(raw, '\n'))
	return err == nil
}

// redisAddr resolves NERVOUS_REDIS_URL (default redis://localhost:6379)
// to host:port. URLs carrying credentials are refused so the caller
// falls back to the nervous CLI, which handles auth.
func redisAddr() (string, error) {
	rawURL := os.Getenv("NERVOUS_REDIS_URL")
	if rawURL == "" {
		rawURL = "redis://localhost:6379"
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", err
	}
	if u.User != nil {
		return "", fmt.Errorf("nbuspub direct: authed redis URL, defer to nervous")
	}
	host := u.Host
	if host == "" {
		host = "localhost:6379"
	}
	if !strings.Contains(host, ":") {
		host += ":6379"
	}
	return host, nil
}

func redisDeliver(env envelope, raw []byte) error {
	addr, err := redisAddr()
	if err != nil {
		return err
	}
	conn, err := net.DialTimeout("tcp", addr, time.Second)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
	r := bufio.NewReader(conn)

	// Idempotent claim, matching nervous: nil-reply = already delivered
	// (skip), infra error on the claim itself is fail-open.
	if env.ID != "" {
		reply, err := respCommand(conn, r,
			"SET", "nbus:dedup:"+env.ID, "1", "NX", "EX", "600")
		if err == nil && reply == "nil" {
			return nil
		}
	}
	fields := []string{
		"_raw", string(raw),
		"type", env.Type,
		"source", env.Source,
		"timestamp", env.Time,
		"event_id", env.ID,
	}
	args := append([]string{"XADD", "nbus:" + env.Type,
		"MAXLEN", "~", "10000", "*"}, fields...)
	if _, err := respCommand(conn, r, args...); err != nil {
		return err
	}
	// nbus:all mirror is best-effort, like both nervous paths.
	allArgs := append([]string{"XADD", "nbus:all",
		"MAXLEN", "~", "50000", "*"}, fields...)
	_, _ = respCommand(conn, r, allArgs...)
	return nil
}

// respCommand writes one RESP array command and reads one reply.
// Returns "nil" for null bulk replies, the raw line otherwise.
func respCommand(conn net.Conn, r *bufio.Reader, args ...string) (string, error) {
	var b strings.Builder
	fmt.Fprintf(&b, "*%d\r\n", len(args))
	for _, a := range args {
		fmt.Fprintf(&b, "$%d\r\n%s\r\n", len(a), a)
	}
	if _, err := conn.Write([]byte(b.String())); err != nil {
		return "", err
	}
	line, err := r.ReadString('\n')
	if err != nil {
		return "", err
	}
	line = strings.TrimRight(line, "\r\n")
	if line == "" {
		return "", fmt.Errorf("empty redis reply")
	}
	switch line[0] {
	case '-':
		return "", fmt.Errorf("redis error: %s", line[1:])
	case '+', ':':
		return line[1:], nil
	case '$':
		if line == "$-1" {
			return "nil", nil
		}
		n := 0
		if _, err := fmt.Sscanf(line[1:], "%d", &n); err != nil {
			return "", err
		}
		buf := make([]byte, n+2)
		if _, err := io.ReadFull(r, buf); err != nil {
			return "", err
		}
		return string(buf[:n]), nil
	default:
		return line, nil
	}
}
