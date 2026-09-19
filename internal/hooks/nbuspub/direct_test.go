package nbuspub

import (
	"bufio"
	"encoding/json"
	"net"
	"os"
	"strconv"
	"strings"
	"testing"
)

// fakeRedis accepts one connection and answers every command with
// canned replies in order, recording the parsed commands.
func fakeRedis(t *testing.T, replies []string) (addr string, got *[][]string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	cmds := &[][]string{}
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		r := bufio.NewReader(conn)
		for _, reply := range replies {
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			if !strings.HasPrefix(line, "*") {
				return
			}
			n, _ := strconv.Atoi(strings.TrimSpace(line[1:]))
			var cmd []string
			for i := 0; i < n; i++ {
				sizeLine, err := r.ReadString('\n')
				if err != nil {
					return
				}
				size, _ := strconv.Atoi(strings.TrimSpace(sizeLine[1:]))
				buf := make([]byte, size+2)
				total := 0
				for total < len(buf) {
					m, err := r.Read(buf[total:])
					total += m
					if err != nil {
						return
					}
				}
				cmd = append(cmd, string(buf[:size]))
			}
			*cmds = append(*cmds, cmd)
			_, _ = conn.Write([]byte(reply))
		}
	}()
	return ln.Addr().String(), cmds
}

func TestPublishDirectDelivers(t *testing.T) {
	addr, got := fakeRedis(t, []string{"+OK\r\n", "$5\r\n1-abc\r\n", "$5\r\n1-abc\r\n"})
	t.Setenv("NERVOUS_REDIS_URL", "redis://"+addr)
	t.Setenv("NERVOUS_DEBUG_LOG", t.TempDir()+"/debug.jsonl")

	err := PublishDirect("bus.test.v1", "/claude-host/tengine", `{"event":"heartbeat"}`)
	if err != nil {
		t.Fatalf("PublishDirect: %v", err)
	}
	cmds := *got
	if len(cmds) != 3 {
		t.Fatalf("want 3 commands (SET + 2 XADD), got %d: %v", len(cmds), cmds)
	}
	if cmds[0][0] != "SET" || !strings.HasPrefix(cmds[0][1], "nbus:dedup:") {
		t.Fatalf("first command not a dedup claim: %v", cmds[0])
	}
	if cmds[1][0] != "XADD" || cmds[1][1] != "nbus:bus.test.v1" {
		t.Fatalf("second command not channel XADD: %v", cmds[1])
	}
	if cmds[2][1] != "nbus:all" {
		t.Fatalf("third command not nbus:all XADD: %v", cmds[2])
	}
	// _raw field must be a valid CloudEvents envelope wrapping the payload.
	var env envelope
	rawIdx := -1
	for i, a := range cmds[1] {
		if a == "_raw" {
			rawIdx = i + 1
		}
	}
	if rawIdx < 0 || json.Unmarshal([]byte(cmds[1][rawIdx]), &env) != nil {
		t.Fatalf("no parseable _raw envelope in XADD: %v", cmds[1])
	}
	if env.Type != "bus.test.v1" || env.Source != "/claude-host/tengine" ||
		env.SpecVersion != "1.0" || string(env.Data) != `{"event":"heartbeat"}` {
		t.Fatalf("envelope mismatch: %+v", env)
	}
}

func TestPublishDirectDedupSkips(t *testing.T) {
	// nil reply on the claim = already delivered — no XADD must follow.
	addr, got := fakeRedis(t, []string{"$-1\r\n"})
	t.Setenv("NERVOUS_REDIS_URL", "redis://"+addr)
	t.Setenv("NERVOUS_DEBUG_LOG", t.TempDir()+"/debug.jsonl")

	if err := PublishDirect("bus.test.v1", "/s", `{"a":1}`); err != nil {
		t.Fatalf("PublishDirect: %v", err)
	}
	if len(*got) != 1 {
		t.Fatalf("dedup hit must stop after SET, got commands: %v", *got)
	}
}

func TestSemanticEventIDIsUsedForDedup(t *testing.T) {
	addr, got := fakeRedis(t, []string{"+OK\r\n", "$5\r\n1-abc\r\n", "$5\r\n1-abc\r\n"})
	t.Setenv("NERVOUS_REDIS_URL", "redis://"+addr)
	t.Setenv("NERVOUS_DEBUG_LOG", t.TempDir()+"/debug.jsonl")
	if err := PublishDirect("bus.test.v1", "/s", `{"event_id":"codex-stable","event":"started"}`); err != nil {
		t.Fatal(err)
	}
	if len(*got) == 0 || (*got)[0][1] != "nbus:dedup:codex-stable" {
		t.Fatalf("want stable claim, got %v", *got)
	}
}

func TestPublishDirectRedisDownIsDurable(t *testing.T) {
	t.Setenv("NERVOUS_REDIS_URL", "redis://127.0.0.1:1") // nothing listens
	log := t.TempDir() + "/debug.jsonl"
	t.Setenv("NERVOUS_DEBUG_LOG", log)

	// Fail-soft: debug.jsonl append succeeded, so this is success.
	if err := PublishDirect("bus.test.v1", "/s", `{"a":1}`); err != nil {
		t.Fatalf("want fail-soft nil, got %v", err)
	}
}

func TestDirectReceiptIsSynced(t *testing.T) {
	log := t.TempDir() + "/debug.jsonl"
	t.Setenv("NERVOUS_DEBUG_LOG", log)
	if !appendDebugLog([]byte(`{"event_id":"receipt-id"}`)) {
		t.Fatal("receipt append failed")
	}
	b, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"receipt-id"`) {
		t.Fatalf("receipt missing: %s", b)
	}
}

func TestDirectReceiptRequiresSync(t *testing.T) {
	old := debugLogSync
	defer func() { debugLogSync = old }()
	debugLogSync = func(*os.File) error { return os.ErrInvalid }
	t.Setenv("NERVOUS_DEBUG_LOG", t.TempDir()+"/debug.jsonl")
	if appendDebugLog([]byte(`{"event_id":"sync"}`)) {
		t.Fatal("unsynced receipt was accepted")
	}
}

func TestPublishDirectRejectsBadPayload(t *testing.T) {
	if err := PublishDirect("bus.test.v1", "/s", "not json"); err == nil {
		t.Fatal("want error on invalid payload")
	}
}
