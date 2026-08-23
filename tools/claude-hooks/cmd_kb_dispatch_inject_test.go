package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ---------- helpers ----------

// kdiWriteFakeKb writes a fake `kb` binary at $HOME/.local/bin/kb whose
// behavior is driven by a small shell script. Mirrors the
// TestResolveKbBinary_PrefersDotLocalBin fixture pattern in
// cmd_kb_prime_siblings_test.go.
func kdiWriteFakeKb(t *testing.T, home, script string) string {
	t.Helper()
	kbDir := filepath.Join(home, ".local", "bin")
	if err := os.MkdirAll(kbDir, 0o755); err != nil {
		t.Fatal(err)
	}
	kbBin := filepath.Join(kbDir, "kb")
	if err := os.WriteFile(kbBin, []byte("#!/bin/sh\n"+script), 0o755); err != nil {
		t.Fatal(err)
	}
	return kbBin
}

// kdiRunWithStdin feeds stdinJSON to cmdKbDispatchInject() and captures
// stdout, mirroring TestCmdBrainRegister_AlwaysPrintsAck's os.Pipe
// technique.
func kdiRunWithStdin(t *testing.T, stdinJSON string) string {
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

	cmdKbDispatchInject()

	_ = wo.Close()
	os.Stdout = oldStdout
	<-done
	_ = ro.Close()

	return strings.TrimSpace(buf.String())
}

func kdiStdinJSON(t *testing.T, toolName, cwd string, toolInput map[string]any) string {
	t.Helper()
	raw, err := json.Marshal(toolInput)
	if err != nil {
		t.Fatal(err)
	}
	payload := map[string]any{
		"session_id": "test-sess",
		"tool_name":  toolName,
		"cwd":        cwd,
		"tool_input": json.RawMessage(raw),
	}
	b, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// ---------- kdiRenderBlock / kdiRenderSection ----------

func TestKdiRenderSection_CapsItemsAndTitleLength(t *testing.T) {
	titles := []string{"a", "b", "c", "d", "e", "f", "g"}
	got := kdiRenderSection("Recent", titles)
	// One bullet is on the same line as the header's trailing newline
	// ("### Recent\n- a"), the rest are each preceded by "\n- ".
	if n := strings.Count(got, "- "); n != kdiMaxItemsPerSection {
		t.Errorf("expected %d bullets, got %d in block:\n%s", kdiMaxItemsPerSection, n, got)
	}
	if !strings.HasPrefix(got, "### Recent\n") {
		t.Errorf("missing header, got: %s", got)
	}
}

func TestKdiRenderSection_EmptyYieldsEmpty(t *testing.T) {
	if got := kdiRenderSection("Recent", nil); got != "" {
		t.Errorf("expected empty, got %q", got)
	}
}

func TestKdiRenderBlock_EmptyResultYieldsEmpty(t *testing.T) {
	if got := kdiRenderBlock(kpsPrimeResult{}); got != "" {
		t.Errorf("expected empty block for empty result, got %q", got)
	}
}

func TestKdiRenderBlock_ContainsMarkerAndSections(t *testing.T) {
	r := kpsPrimeResult{
		ActivePlans: []kpsPrimeEntry{{Title: "Wave 5 rollout"}},
		PendingVets: []string{"vet-1", "vet-2"},
		Recent:      []kpsPrimeEntry{{Title: "Landed migration 583"}},
		Gaps:        []kpsPrimeEntry{{Title: "No stop-loss test"}},
	}
	got := kdiRenderBlock(r)
	if !strings.Contains(got, kdiMarker) {
		t.Errorf("block missing marker: %s", got)
	}
	for _, want := range []string{"Wave 5 rollout", "Pending Vets (2)", "vet-1", "Landed migration 583", "No stop-loss test"} {
		if !strings.Contains(got, want) {
			t.Errorf("block missing %q:\n%s", want, got)
		}
	}
}

func TestKdiRenderBlock_BoundedByCapBytes(t *testing.T) {
	var plans []kpsPrimeEntry
	for i := 0; i < 500; i++ {
		plans = append(plans, kpsPrimeEntry{Title: strings.Repeat("x", 90)})
	}
	r := kpsPrimeResult{ActivePlans: plans, Recent: plans, Gaps: plans}
	got := kdiRenderBlock(r)
	if len(got) > kdiBlockCapBytes {
		t.Errorf("block exceeds cap: %d bytes > %d", len(got), kdiBlockCapBytes)
	}
}

// ---------- kscBuildLandmarkArgs-style pure logic already covered ----------
// (see cmd_kb_subagent_capture_test.go)

// ---------- cmdKbDispatchInject integration ----------

func TestCmdKbDispatchInject_SkipsWorkflow(t *testing.T) {
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)
	kdiWriteFakeKb(t, fakeHome, "exit 1\n") // would fail loudly if ever invoked

	stdin := kdiStdinJSON(t, "Workflow", fakeHome, map[string]any{"script": "isolation: 'worktree'"})
	out := kdiRunWithStdin(t, stdin)
	if out != "" {
		t.Errorf("expected no output for Workflow, got %q", out)
	}
}

func TestCmdKbDispatchInject_SkipsMechanicalSubagentType(t *testing.T) {
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)
	kdiWriteFakeKb(t, fakeHome, kdiFakeKbScript(t, `{"active_plans":[{"title":"should not appear"}]}`))

	stdin := kdiStdinJSON(t, "Agent", fakeHome, map[string]any{
		"prompt":        "do the thing",
		"subagent_type": "haiku-grunt",
	})
	out := kdiRunWithStdin(t, stdin)
	if out != "" {
		t.Errorf("expected no output for haiku-grunt subagent_type, got %q", out)
	}
}

func TestCmdKbDispatchInject_NoDoubleInject(t *testing.T) {
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)
	kdiWriteFakeKb(t, fakeHome, "exit 1\n") // would fail loudly if ever invoked

	prompt := "## " + kdiMarker + "\n\nsome prior context\n\ndo the thing"
	stdin := kdiStdinJSON(t, "Agent", fakeHome, map[string]any{"prompt": prompt})
	out := kdiRunWithStdin(t, stdin)
	if out != "" {
		t.Errorf("expected no output when marker already present, got %q", out)
	}
}

func TestCmdKbDispatchInject_FailOpenOnKbTimeout(t *testing.T) {
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)
	// kb brief --help fails fast (fallback to prime), kb prime hangs past
	// the overall hook timeout.
	kdiWriteFakeKb(t, fakeHome, `
if [ "$1" = "brief" ]; then exit 1; fi
sleep 5
`)

	start := time.Now()
	stdin := kdiStdinJSON(t, "Agent", fakeHome, map[string]any{"prompt": "do the thing"})
	out := kdiRunWithStdin(t, stdin)
	elapsed := time.Since(start)

	if out != "" {
		t.Errorf("expected fail-open (no output) on kb timeout, got %q", out)
	}
	if elapsed > kdiOverallTimeout+2*time.Second {
		t.Errorf("hook did not respect overall timeout: took %v", elapsed)
	}
}

func TestCmdKbDispatchInject_FailOpenOnKbBadJSON(t *testing.T) {
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)
	kdiWriteFakeKb(t, fakeHome, `
if [ "$1" = "brief" ]; then exit 1; fi
echo 'not json'
`)
	stdin := kdiStdinJSON(t, "Agent", fakeHome, map[string]any{"prompt": "do the thing"})
	out := kdiRunWithStdin(t, stdin)
	if out != "" {
		t.Errorf("expected fail-open on malformed kb JSON, got %q", out)
	}
}

func TestCmdKbDispatchInject_InjectsAndPrependsBlock(t *testing.T) {
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)
	kdiWriteFakeKb(t, fakeHome, kdiFakeKbScript(t, `{"active_plans":[{"title":"Wave 5 rollout"}]}`))

	stdin := kdiStdinJSON(t, "Task", fakeHome, map[string]any{"prompt": "do the thing"})
	out := kdiRunWithStdin(t, stdin)
	if out == "" {
		t.Fatal("expected updatedInput output, got nothing")
	}

	var resp map[string]any
	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		t.Fatalf("output not valid JSON: %v\noutput: %s", err, out)
	}
	hso, ok := resp["hookSpecificOutput"].(map[string]any)
	if !ok {
		t.Fatalf("missing hookSpecificOutput: %v", resp)
	}
	if hso["hookEventName"] != "PreToolUse" {
		t.Errorf("hookEventName = %v, want PreToolUse", hso["hookEventName"])
	}
	updated, ok := hso["updatedInput"].(map[string]any)
	if !ok {
		t.Fatalf("missing updatedInput: %v", hso)
	}
	prompt, _ := updated["prompt"].(string)
	if !strings.Contains(prompt, kdiMarker) {
		t.Errorf("updated prompt missing marker: %s", prompt)
	}
	if !strings.Contains(prompt, "Wave 5 rollout") {
		t.Errorf("updated prompt missing kb content: %s", prompt)
	}
	if !strings.HasSuffix(prompt, "do the thing") {
		t.Errorf("original prompt should be preserved at the end: %s", prompt)
	}
}

// kdiFakeKbScript builds a fake-kb shell script that: fails `brief --help`
// (forcing fallback to prime) and echoes the given JSON body for any
// `prime --project ... --json` call.
func kdiFakeKbScript(t *testing.T, primeJSON string) string {
	t.Helper()
	return "if [ \"$1\" = \"brief\" ]; then exit 1; fi\ncat <<'EOF'\n" + primeJSON + "\nEOF\n"
}
