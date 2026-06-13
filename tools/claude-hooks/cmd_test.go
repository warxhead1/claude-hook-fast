package main

import (
	"testing"
)

func TestContainsAny(t *testing.T) {
	if !containsAny("WriteFile", "Write", "Edit") {
		t.Error("containsAny should return true for WriteFile with Write/Edit substrings")
	}
	if !containsAny("EditFile", "Write", "Edit") {
		t.Error("containsAny should return true for EditFile with Write/Edit substrings")
	}
	if containsAny("ReadFile", "Write", "Edit") {
		t.Error("containsAny should return false for ReadFile without Write/Edit substrings")
	}
	if !containsAny("MultiEditFile", "Write", "Edit", "MultiEdit") {
		t.Error("containsAny should return true for MultiEditFile with MultiEdit substring")
	}
}
