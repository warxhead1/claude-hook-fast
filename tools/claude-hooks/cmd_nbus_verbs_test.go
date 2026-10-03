package main

import "testing"

func TestBashVerbs_RedirectionsAndSubstitutions(t *testing.T) {
	cases := []struct{ cmd, want string }{
		{`redis-cli XLEN s 2>&1 | grep -c x`, "redis-cli grep"},
		{`make test &>out.log; echo done`, "make echo"},
		{`cat <&0 | wc -l`, "cat wc"},
		{`R=$(command -v redis-cli || command -v valkey-cli); echo "cli=$R"`, "echo"},
		{"X=`which go` && go build", "go"},
		{`GOFLAGS=$FLAGS go test ./...`, "go"},
		{`sleep 5 & wait`, "sleep wait"},
		{`-v foo`, ""},
	}
	for _, c := range cases {
		if got := bashVerbs(c.cmd); got != c.want {
			t.Errorf("bashVerbs(%q) = %q, want %q", c.cmd, got, c.want)
		}
	}
}
