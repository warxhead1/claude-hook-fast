package main

import "testing"

// TestSplitShellSegmentsQuoteAware reproduces the 2026-08-05 false block:
// a multi-line double-quoted SQL string piped to a wrapper was split at the
// quoted newlines/semicolons by the old segSplitRe, and `> 0` inside the
// quotes parsed as an output redirect to a file named "0".
func TestSplitShellSegmentsQuoteAware(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{
			name: "quoted newline and semicolon never split",
			in:   "echo \"SELECT x\nFROM t WHERE n > 0;\" | run.sh",
			want: []string{"echo \"SELECT x\nFROM t WHERE n > 0;\" | run.sh"},
		},
		{
			name: "single-quoted separators never split",
			in:   "echo 'a && b; c' && ls",
			want: []string{"echo 'a && b; c' ", " ls"},
		},
		{
			name: "unquoted separators still split",
			in:   "a; b && c || d\ne",
			want: []string{"a", " b ", " c ", " d", "e"},
		},
		{
			name: "single pipe is not a separator",
			in:   "a | b",
			want: []string{"a | b"},
		},
		{
			name: "escaped quote outside quotes",
			in:   `echo \" ; ls`,
			want: []string{`echo \" `, ` ls`},
		},
	}
	for _, tc := range cases {
		got := splitShellSegments(tc.in)
		if len(got) != len(tc.want) {
			t.Errorf("%s: got %d segments %q, want %d %q", tc.name, len(got), got, len(tc.want), tc.want)
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("%s: segment %d = %q, want %q", tc.name, i, got[i], tc.want[i])
			}
		}
	}
}
