package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// ---------- kscBuildLandmarkArgs ----------

func TestKscBuildLandmarkArgs_NormalCase(t *testing.T) {
	files := []string{"a.go", "b.go"}
	args := kscBuildLandmarkArgs("sonnet-worker", "kb", files)

	want := []string{
		"landmark", "Subagent sonnet-worker: 2 files touched — a.go, b.go",
		"--project", "kb",
		"--files", "a.go,b.go",
		"--status", "inbox",
		"--confidence", "0.3",
		"--json",
	}
	got := args
	if len(got) != len(want) {
		t.Fatalf("got %v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("arg[%d] = %q want %q", i, got[i], want[i])
		}
	}
}

func TestKscBuildLandmarkArgs_EmptySubagentTypeFallsBackToUnknown(t *testing.T) {
	args := kscBuildLandmarkArgs("", "proj", []string{"x"})
	if !strings.Contains(args[1], "Subagent unknown:") {
		t.Errorf("expected 'unknown' fallback in title, got %q", args[1])
	}
}

func TestKscBuildLandmarkArgs_TitlePreviewCappedAtThree(t *testing.T) {
	files := []string{"a", "b", "c", "d", "e"}
	args := kscBuildLandmarkArgs("haiku-grunt", "proj", files)
	title := args[1]
	if !strings.Contains(title, "5 files touched") {
		t.Errorf("title should report full count: %q", title)
	}
	if strings.Contains(title, ", d") || strings.Contains(title, ", e") {
		t.Errorf("title preview should cap at 3 paths: %q", title)
	}
	// --files (the actual landmark payload) still carries all 5.
	idx := -1
	for i, a := range args {
		if a == "--files" {
			idx = i
			break
		}
	}
	if idx == -1 || args[idx+1] != "a,b,c,d,e" {
		t.Errorf("--files should carry all files uncapped, got %q", args[idx+1])
	}
}

// ---------- cmdKbSubagentCapture integration ----------

// kscRunWithStdin feeds stdinJSON to cmdKbSubagentCapture() and captures
// stdout. Mirrors kdiRunWithStdin (cmd_kb_dispatch_inject_test.go) /
// TestCmdBrainRegister_AlwaysPrintsAck's os.Pipe technique.
func kscRunWithStdin(t *testing.T, stdinJSON string) string {
	t.Helper()

	oldStdin := os.Stdin
	r, w, _ := os.Pipe()
	os.Stdin = r
	_, _ = w.WriteString(stdinJSON)
	_ = w.Close()
	defer func() { os.Stdin = oldStdin }()

	oldStdout := os.Stdout
	ro, wo, _ := os.Pipe()
	os.Stdout = wo

	done := make(chan struct{})
	var buf strings.Builder
	go func() {
		tmp := make([]byte, 4096)
		for {
			n, err := ro.Read(tmp)
			if n > 0 {
				buf.Write(tmp[:n])
			}
			if err != nil {
				break
			}
		}
		close(done)
	}()

	cmdKbSubagentCapture()

	_ = wo.Close()
	os.Stdout = oldStdout
	<-done
	_ = ro.Close()

	return strings.TrimSpace(buf.String())
}

// kscInitRepoWithChange creates a git repo with one committed file and
// one dirty (touched) file, so kicTouchedFiles has something to report.
func kscInitRepoWithChange(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q")
	run("config", "user.email", "test@test.local")
	run("config", "user.name", "test")
	if err := os.WriteFile(filepath.Join(dir, "committed.txt"), []byte("v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "committed.txt")
	run("commit", "-q", "-m", "init")
	if err := os.WriteFile(filepath.Join(dir, "touched.go"), []byte("package x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestCmdKbSubagentCapture_AlwaysEmitsAck(t *testing.T) {
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)
	// No kb binary, no touched files (empty cwd) — still must ack.

	stdin := `{"session_id":"s1","cwd":"","subagent_type":"sonnet-worker"}`
	out := kscRunWithStdin(t, stdin)
	if out == "" {
		t.Fatal("expected SubagentStop ack, got nothing")
	}
	var ack map[string]any
	if err := json.Unmarshal([]byte(out), &ack); err != nil {
		t.Fatalf("ack not valid JSON: %v\noutput: %s", err, out)
	}
	hso, ok := ack["hookSpecificOutput"].(map[string]any)
	if !ok || hso["hookEventName"] != "SubagentStop" {
		t.Errorf("unexpected ack shape: %v", ack)
	}
}

func TestCmdKbSubagentCapture_SkipsUnresolvableProject(t *testing.T) {
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)

	stdin := `{"session_id":"s1","cwd":"","subagent_type":"sonnet-worker"}`
	out := kscRunWithStdin(t, stdin)
	// Still acks (verified above), but must not have shelled out to a
	// nonexistent kb binary — no way to assert that directly here beyond
	// "didn't hang/panic", which the surrounding test run already proves.
	if out == "" {
		t.Fatal("expected ack even when skipping capture")
	}
}

func TestCmdKbSubagentCapture_RunsLandmarkOnTouchedFiles(t *testing.T) {
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)

	repo := kscInitRepoWithChange(t)

	marker := filepath.Join(fakeHome, "landmark-called.txt")
	script := "#!/bin/sh\n" +
		"if [ \"$1\" = \"landmark\" ]; then echo \"$@\" > " + marker + "; fi\n" +
		"exit 0\n"
	kbDir := filepath.Join(fakeHome, ".local", "bin")
	if err := os.MkdirAll(kbDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(kbDir, "kb"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	payload := map[string]any{
		"session_id":    "s1",
		"cwd":           repo,
		"subagent_type": "sonnet-worker",
	}
	raw, _ := json.Marshal(payload)

	out := kscRunWithStdin(t, string(raw))
	if out == "" {
		t.Fatal("expected ack")
	}

	data, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("expected kb landmark to have been invoked, marker file missing: %v", err)
	}
	call := string(data)
	if !strings.Contains(call, "touched.go") {
		t.Errorf("landmark call missing touched file: %q", call)
	}
	if !strings.Contains(call, "Subagent sonnet-worker:") {
		t.Errorf("landmark call missing subagent-type title prefix: %q", call)
	}
	if !strings.Contains(call, "--status inbox") {
		t.Errorf("landmark call missing --status inbox: %q", call)
	}
}
