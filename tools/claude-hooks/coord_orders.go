package main

// coord_orders.go — standing orders (docs/coordinator-inbox.md, component C):
// repeat lane questions that already have a ruling are answered by coord-wake
// itself (auto = true) or annotated with the suggested reply (auto = false).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

type coordOrder struct {
	ID    string   `toml:"id"`
	Match string   `toml:"match"`
	Types []string `toml:"types"`
	Reply string   `toml:"reply"`
	Auto  bool     `toml:"auto"`
	re    *regexp.Regexp
}

type coordOrders struct {
	IntegrationBranch string        `toml:"integration_branch"`
	Reap              string        `toml:"reap"`               // auto | suggest | off (default auto)
	ReapGraceMinutes  int           `toml:"reap_grace_minutes"` // default 30
	Orders            []*coordOrder `toml:"order"`
}

// coordRepoToplevel returns the git toplevel of dir (cwd when empty), or "".
func coordRepoToplevel(dir string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "rev-parse", "--show-toplevel")
	if dir != "" {
		cmd.Dir = dir
	}
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// coordLoadOrders reads <repo>/.orca/standing-orders.toml, falling back to
// <stateDir>/standing-orders.toml. Orders with a bad regexp are dropped.
func coordLoadOrders(repoDir, stateDir string) *coordOrders {
	var paths []string
	if repoDir != "" {
		paths = append(paths, filepath.Join(repoDir, ".orca", "standing-orders.toml"))
	}
	paths = append(paths, filepath.Join(stateDir, "standing-orders.toml"))
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var o coordOrders
		if toml.Unmarshal(b, &o) != nil {
			continue
		}
		kept := o.Orders[:0]
		for _, od := range o.Orders {
			re, err := regexp.Compile(od.Match)
			if err != nil || od.Reply == "" {
				continue
			}
			od.re = re
			if len(od.Types) == 0 {
				od.Types = []string{"question"}
			}
			kept = append(kept, od)
		}
		o.Orders = kept
		return &o
	}
	return &coordOrders{}
}

// find returns the first order whose type list and pattern match m.
func (o *coordOrders) find(m coordMsg) *coordOrder {
	if o == nil {
		return nil
	}
	hay := m.Subject + "\n" + m.Body
	for _, od := range o.Orders {
		typeOK := false
		for _, t := range od.Types {
			if t == m.Type {
				typeOK = true
			}
		}
		if typeOK && od.re.MatchString(hay) {
			return od
		}
	}
	return nil
}

func coordRepoDir(cfg coordConfig) string {
	if cfg.RepoDir != "" {
		return cfg.RepoDir
	}
	return coordRepoToplevel("")
}

func coordIndent(s string) string {
	return "  " + strings.ReplaceAll(strings.TrimRight(s, "\n"), "\n", "\n  ") + "\n"
}

// coordApplyOrders answers auto orders and annotates suggest-only orders.
// It returns the messages that still need a wake, plus per-message notes.
// A failed auto reply keeps the message in the wake, with the suggestion.
func coordApplyOrders(ctx context.Context, cfg coordConfig, dir string, l *coordLedger,
	orders *coordOrders, msgs []coordMsg) ([]coordMsg, map[string]string) {
	notes := map[string]string{}
	var keep []coordMsg
	for _, m := range msgs {
		od := orders.find(m)
		if od == nil {
			keep = append(keep, m)
			continue
		}
		if od.Auto {
			if err := coordAutoReply(ctx, cfg, m, od); err == nil {
				l.recordAuto(m, od.ID)
				coordLog(dir, "auto-reply id=%s order=%s lane=%s", m.ID, od.ID, m.From)
				continue
			} else {
				coordLog(dir, "auto-reply FAILED id=%s order=%s: %v", m.ID, od.ID, err)
				notes[m.ID] = fmt.Sprintf("standing order %s auto-reply failed (%v); suggests: %s\n", od.ID, err, od.Reply)
			}
		} else {
			notes[m.ID] = fmt.Sprintf("standing order %s suggests: %s\n", od.ID, od.Reply)
		}
		keep = append(keep, m)
	}
	return keep, notes
}

// coordAutoReply sends the order's reply via orchestration reply.
func coordAutoReply(ctx context.Context, cfg coordConfig, m coordMsg, od *coordOrder) error {
	out, err := coordOrca(ctx, cfg.OrcaBin, "orchestration", "reply", "--id", m.ID, "--body", od.Reply)
	if err != nil {
		return err
	}
	var r struct {
		OK *bool `json:"ok"`
	}
	if json.Unmarshal(out, &r) == nil && r.OK != nil && !*r.OK {
		return errors.New("orca reply not ok")
	}
	return nil
}
