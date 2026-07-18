# claude-hook-fast — build/test/install the hook binaries invoked by every
# live Claude Code and Codex session.
#
# `make install` is live-safe: it gates on `go test`, builds each binary to a
# temp name in $(BIN), backs up the currently-running binary, then renames the
# new one over it. A rename survives a running executable; a plain `cp` over it
# would risk ETXTBSY (text file busy) while a session is mid-hook. Temp and
# destination share $(BIN) so the rename is atomic (same filesystem).
#
# Override the destination with `make install BIN=/somewhere/bin`.

BIN ?= $(HOME)/.local/bin
GO  ?= go
TS  := $(shell date +%Y%m%d-%H%M%S)

.PHONY: build test install install-claude-hook-fast install-codex-hook \
        install-hearth-loom-hook install-steward clean-baks

build:
	$(GO) build ./...

test:
	$(GO) test ./...

# Default install = the two binaries actually deployed to $(BIN):
# claude-hook-fast (CC hooks) and codex-hook (Codex hooks). Gated on tests so a
# broken tree never reaches every live session. hearth-loom-hook is opt-in
# (install-hearth-loom-hook) — it is not deployed to $(BIN) today.
install: test install-claude-hook-fast install-codex-hook
	@echo "installed: claude-hook-fast, codex-hook -> $(BIN)"

# --- per-binary atomic installs (main package -> installed name) ---

install-claude-hook-fast:
	@mkdir -p "$(BIN)"
	$(GO) build -o "$(BIN)/.claude-hook-fast.new" ./tools/claude-hooks
	@[ -f "$(BIN)/claude-hook-fast" ] && cp -p "$(BIN)/claude-hook-fast" "$(BIN)/claude-hook-fast.bak-$(TS)" || true
	mv -f "$(BIN)/.claude-hook-fast.new" "$(BIN)/claude-hook-fast"
	@echo "  claude-hook-fast -> $(BIN)/claude-hook-fast"

install-codex-hook:
	@mkdir -p "$(BIN)"
	$(GO) build -o "$(BIN)/.codex-hook.new" ./cmd/codex-hook
	@[ -f "$(BIN)/codex-hook" ] && cp -p "$(BIN)/codex-hook" "$(BIN)/codex-hook.bak-$(TS)" || true
	mv -f "$(BIN)/.codex-hook.new" "$(BIN)/codex-hook"
	@echo "  codex-hook -> $(BIN)/codex-hook"

install-hearth-loom-hook:
	@mkdir -p "$(BIN)"
	$(GO) build -o "$(BIN)/.hearth-loom-hook.new" ./cmd/hearth-loom-hook
	@[ -f "$(BIN)/hearth-loom-hook" ] && cp -p "$(BIN)/hearth-loom-hook" "$(BIN)/hearth-loom-hook.bak-$(TS)" || true
	mv -f "$(BIN)/.hearth-loom-hook.new" "$(BIN)/hearth-loom-hook"
	@echo "  hearth-loom-hook -> $(BIN)/hearth-loom-hook"

install-steward: test
	@mkdir -p "$(BIN)"
	$(GO) build -trimpath -ldflags='-s -w' -o "$(BIN)/claude-steward.new.$$$$" ./cmd/claude-steward
	@[ -f "$(BIN)/claude-steward" ] && cp -p "$(BIN)/claude-steward" "$(BIN)/claude-steward.bak-$(TS)" || true
	mv -f "$(BIN)/claude-steward.new.$$$$" "$(BIN)/claude-steward"
	@echo "  claude-steward -> $(BIN)/claude-steward"

# Housekeeping: prune install backups older than 14 days.
clean-baks:
	@find "$(BIN)" -maxdepth 1 -name '*.bak-*' -mtime +14 -print -delete 2>/dev/null || true
