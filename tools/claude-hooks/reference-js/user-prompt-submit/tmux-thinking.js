#!/usr/bin/env node

/**
 * TMux Thinking Hook - Shows when user submits a prompt
 */

const { execSync } = require('child_process');
const fs = require('fs');
const { resolveWindow } = require('../lib/tmux-window-resolver');
const { alerter } = require('../lib/approval-alerter-simple');
const { updateTmuxWindow } = require('../lib/tmux-status-coordinator');

function main() {
    try {
        if (!process.env.TMUX) return;

        const windowId = resolveWindow() || 'current';

        // Stop any active approval alert when user submits a new prompt
        alerter.stopAlert(windowId);

        fs.appendFileSync('/tmp/tmux-hook.log', `${new Date().toISOString()} - UserPromptSubmit: thinking\n`);

        // Set to thinking state when user submits prompt using coordinator
        updateTmuxWindow("⚡ thinking", windowId, 'user-prompt-submit');
    } catch (error) {
        // Silent fail
    }

    // UserPromptSubmit hooks don't need JSON output - stdout goes to context
}

main();