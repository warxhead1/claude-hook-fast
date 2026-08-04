package main

import (
	"reflect"
	"strings"
	"testing"
)

// ---------- kicBuildLandmarkArgs ----------

func TestKicBuildLandmarkArgs_NormalCase(t *testing.T) {
	files := []string{"a.go", "b.go"}
	args := kicBuildLandmarkArgs("nervous-bus", files)

	want := []string{
		"landmark", "Session: 2 files touched in nervous-bus",
		"--project", "nervous-bus",
		"--files", "a.go,b.go",
		"--status", "inbox",
		"--confidence", "0.3",
		"--json",
	}
	if !reflect.DeepEqual(args, want) {
		t.Errorf("got %v want %v", args, want)
	}
}

func TestKicBuildLandmarkArgs_TitleReflectsCount(t *testing.T) {
	files := []string{"x", "y", "z"}
	args := kicBuildLandmarkArgs("kb", files)
	if args[1] != "Session: 3 files touched in kb" {
		t.Errorf("unexpected title: %q", args[1])
	}
}

// ---------- kicTouchedFiles ----------

func TestKicTouchedFiles_EmptyCWD(t *testing.T) {
	if got := kicTouchedFiles(""); got != nil {
		t.Errorf("expected nil for empty cwd, got %v", got)
	}
}

func TestKicTouchedFiles_NonRepoDir(t *testing.T) {
	// /tmp is (almost certainly) not a git repo; git status should error
	// and kicTouchedFiles must return nil, not panic or hang.
	got := kicTouchedFiles("/tmp")
	if got != nil {
		t.Errorf("expected nil for non-repo dir, got %v", got)
	}
}

// ---------- cmdKbInboxCapture integration-ish behavior via helpers ----------
// cmdKbInboxCapture itself does I/O (stdin, exec), so we test its
// component decisions (the pure functions above) plus the skip
// conditions directly, mirroring cmd_brain_register_test.go's style of
// testing extracted logic rather than the full hook entrypoint.

func TestKicRunLandmark_EmptyBinaryNoop(t *testing.T) {
	// Must not panic/hang when binary is "" — this is the
	// resolveKbBinary()=="" skip path.
	kicRunLandmark("", []string{"landmark", "x"})
}

func TestKicBuildLandmarkArgs_FilesJoinedWithCommas(t *testing.T) {
	args := kicBuildLandmarkArgs("proj", []string{"one", "two", "three"})
	idx := -1
	for i, a := range args {
		if a == "--files" {
			idx = i
			break
		}
	}
	if idx == -1 || idx+1 >= len(args) {
		t.Fatal("--files flag not found")
	}
	if !strings.Contains(args[idx+1], "one,two,three") {
		t.Errorf("unexpected --files value: %q", args[idx+1])
	}
}
