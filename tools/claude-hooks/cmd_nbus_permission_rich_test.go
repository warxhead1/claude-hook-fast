package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestPermissionRichFieldsBash(t *testing.T) {
	raw := json.RawMessage(`{"command":"git push origin main\n# second line","description":"Push the branch"}`)
	got := permissionRichFields("Bash", raw)
	if got["command_full"] != "git push origin main\n# second line" {
		t.Fatalf("command_full = %v", got["command_full"])
	}
	if got["description"] != "Push the branch" {
		t.Fatalf("description = %v", got["description"])
	}
	if _, ok := got["tool_input"].(json.RawMessage); !ok {
		t.Fatalf("tool_input missing/raw: %T", got["tool_input"])
	}
	// The envelope must marshal (RawMessage survives json.Marshal).
	if _, err := json.Marshal(got); err != nil {
		t.Fatalf("marshal: %v", err)
	}
}

func TestPermissionRichFieldsCapsOversize(t *testing.T) {
	big := strings.Repeat("x", permissionRichFieldsMax+10)
	raw, _ := json.Marshal(map[string]string{"command": big})
	got := permissionRichFields("Bash", raw)
	if len(got["command_full"].(string)) != permissionRichFieldsMax {
		t.Fatalf("command_full not capped")
	}
	if got["command_full_truncated"] != true || got["tool_input_truncated"] != true {
		t.Fatalf("truncation flags missing: %v", got)
	}
	if _, ok := got["tool_input"]; ok {
		t.Fatalf("oversize tool_input should be dropped")
	}
}

func TestPermissionRichFieldsEditUsesPath(t *testing.T) {
	got := permissionRichFields("Edit", json.RawMessage(`{"file_path":"/a/b.rs","old_string":"x"}`))
	if got["command_full"] != "/a/b.rs" {
		t.Fatalf("command_full = %v", got["command_full"])
	}
	if _, ok := got["description"]; ok {
		t.Fatalf("no description expected")
	}
}
