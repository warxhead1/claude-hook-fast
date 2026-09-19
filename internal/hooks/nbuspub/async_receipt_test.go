package nbuspub

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestPublishAsyncDoesNotSyncOrDeliverInline(t *testing.T) {
	old := debugLogSync
	defer func() { debugLogSync = old }()
	debugLogSync = func(*os.File) error {
		t.Fatal("interactive publisher attempted disk sync")
		return nil
	}
	for _, failure := range []string{"", "receipt", "executable", "start"} {
		t.Run(failure, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "debug.jsonl")
			if failure == "receipt" {
				path = t.TempDir() // opening a directory for append must fail
			}
			t.Setenv("NERVOUS_DEBUG_LOG", path)
			executable := func() (string, error) {
				if failure == "executable" {
					return "", errors.New("executable unavailable")
				}
				return "/hook", nil
			}
			started := false
			err := publishAsync("bus.test.v1", "/test", `{"message":"receipt"}`, executable, func(cmd *exec.Cmd) error {
				started = true
				if len(cmd.ExtraFiles) != 1 {
					t.Fatal("receipt descriptor missing")
				}
				b, err := os.ReadFile(path)
				if err != nil || string(b) != cmd.Args[4]+"\n" {
					t.Fatalf("worker bytes differ from the pre-detach receipt: %q, %v", b, err)
				}
				if failure == "start" {
					return errors.New("fork unavailable")
				}
				return nil
			})
			if (err != nil) != (failure != "") {
				t.Fatalf("failure %q returned %v", failure, err)
			}
			if started != (failure == "" || failure == "start") {
				t.Fatalf("unexpected worker start for %q", failure)
			}
		})
	}
}

// Exercise actual exec inheritance, parent fd closure, delayed sync and log
// rotation. The child cannot finish until after PublishAsync has returned.
func TestAsyncReceiptSurvivesRotationWhileWorkerSyncIsDelayed(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "debug.jsonl")
	t.Setenv("NERVOUS_DEBUG_LOG", path)
	t.Setenv("NERVOUS_NO_REDIS", "1")
	t.Setenv("NBUS_TEST_RECEIPT_WORKER", dir)
	t.Setenv("HEARTH_LOOM_HOST_HOOK_LOG", filepath.Join(dir, "worker.log"))
	defer func() { _ = os.WriteFile(filepath.Join(dir, "release"), nil, 0o600) }()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	err = publishAsync("bus.test.v1", "/test", `{"message":"persist once"}`, func() (string, error) { return exe, nil }, func(cmd *exec.Cmd) error {
		cmd.Args = append([]string{exe, "-test.run=^TestReceiptWorkerHelper$", "--"}, cmd.Args[2:]...)
		return StartDetached(cmd)
	})
	if err != nil {
		t.Fatal(err)
	}
	waitForReceiptFile(t, filepath.Join(dir, "sync-started"))
	if _, err := os.Stat(filepath.Join(dir, "done")); !os.IsNotExist(err) {
		t.Fatal("worker finished before sync was released")
	}
	if err := os.Rename(path, path+".1"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("replacement\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "release"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	waitForReceiptFile(t, filepath.Join(dir, "done"))
	log, _ := os.ReadFile(path + ".1")
	if bytes.Count(log, []byte("\n")) != 1 {
		t.Fatalf("want one receipt, got %q", log)
	}
	if b, _ := os.ReadFile(path); string(b) != "replacement\n" {
		t.Fatalf("worker changed the replacement log: %q", b)
	}
}

func waitForReceiptFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("worker did not create %s", path)
}

func TestReceiptWorkerHelper(t *testing.T) {
	dir := os.Getenv("NBUS_TEST_RECEIPT_WORKER")
	if dir == "" {
		return
	}
	debugLogSync = func(f *os.File) error {
		if err := os.WriteFile(filepath.Join(dir, "sync-started"), nil, 0o600); err != nil {
			return err
		}
		waitForReceiptFile(t, filepath.Join(dir, "release"))
		got, err := f.Stat()
		if err != nil {
			return err
		}
		want, err := os.Stat(filepath.Join(dir, "debug.jsonl.1"))
		if err != nil || !os.SameFile(got, want) {
			return fmt.Errorf("worker did not retain original receipt inode: %v", err)
		}
		return f.Sync()
	}
	WorkerMain(os.Args[len(os.Args)-3:])
	if b, _ := os.ReadFile(filepath.Join(dir, "worker.log")); len(b) != 0 {
		t.Fatalf("worker failed: %s", b)
	}
	if err := os.WriteFile(filepath.Join(dir, "done"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestReceiptWorkerSyncFailureDoesNotReachRedis(t *testing.T) {
	t.Setenv("NERVOUS_NO_REDIS", "")
	t.Setenv("NERVOUS_DEBUG_LOG", filepath.Join(t.TempDir(), "debug.jsonl"))
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	t.Setenv("NERVOUS_REDIS_URL", "redis://"+ln.Addr().String())
	old := debugLogSync
	defer func() { debugLogSync = old }()
	debugLogSync = func(*os.File) error { return errors.New("disk sync failed") }
	raw, _ := prepareEnvelope("bus.test.v1", "/test", `{}`)
	f, err := openDebugReceipt(raw)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if err := syncAndDeliverReceipt(f, raw); err == nil || !strings.Contains(err.Error(), "disk sync failed") {
		t.Fatalf("sync failure not reported: %v", err)
	}
	_ = ln.(*net.TCPListener).SetDeadline(time.Now().Add(20 * time.Millisecond))
	if conn, err := ln.Accept(); err == nil {
		_ = conn.Close()
		t.Fatal("worker contacted Redis despite failed sync")
	}
}

func TestReceiptWorkerDeliversExactEnvelopeAfterSync(t *testing.T) {
	for _, payload := range []string{`{"event":"heartbeat"}`, `{"event_id":"stable-id"}`} {
		t.Run(payload, func(t *testing.T) {
			t.Setenv("NERVOUS_NO_REDIS", "")
			t.Setenv("NERVOUS_DEBUG_LOG", filepath.Join(t.TempDir(), "debug.jsonl"))
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = ln.Close() }()
			t.Setenv("NERVOUS_REDIS_URL", "redis://"+ln.Addr().String())
			synced := make(chan struct{})
			observed := make(chan [][]string, 1)
			go func() {
				conn, err := ln.Accept()
				if err != nil {
					observed <- nil
					return
				}
				defer func() { _ = conn.Close() }()
				_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
				select {
				case <-synced:
				default:
					observed <- nil
					return
				}
				r := bufio.NewReader(conn)
				var cmds [][]string
				for i := 0; i < 3; i++ {
					line, _ := r.ReadString('\n')
					n, _ := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(line, "*")))
					var cmd []string
					for j := 0; j < n; j++ {
						line, _ = r.ReadString('\n')
						size, _ := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(line, "$")))
						b := make([]byte, size+2)
						_, _ = io.ReadFull(r, b)
						cmd = append(cmd, string(b[:size]))
					}
					cmds = append(cmds, cmd)
					_, _ = io.WriteString(conn, "+OK\r\n")
				}
				observed <- cmds
			}()
			old := debugLogSync
			defer func() { debugLogSync = old }()
			debugLogSync = func(f *os.File) error {
				if err := f.Sync(); err != nil {
					return err
				}
				close(synced)
				return nil
			}
			raw, _ := prepareEnvelope("bus.test.v1", "/test", payload)
			f, err := openDebugReceipt(raw)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = f.Close() }()
			if err := syncAndDeliverReceipt(f, raw); err != nil {
				t.Fatal(err)
			}
			cmds := <-observed
			var env envelope
			_ = json.Unmarshal(raw, &env)
			if len(cmds) != 3 || len(cmds[0]) < 2 || cmds[0][1] != "nbus:dedup:"+env.ID {
				t.Fatalf("missing sync/dedup/delivery: %v", cmds)
			}
			for _, cmd := range cmds[1:] {
				found := false
				for i := 0; i+1 < len(cmd); i++ {
					if cmd[i] == "_raw" {
						found = cmd[i+1] == string(raw)
					}
				}
				if !found {
					t.Fatal("Redis envelope differs from persisted bytes")
				}
			}
		})
	}
}
