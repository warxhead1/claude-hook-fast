#!/usr/bin/env node

/**
 * TMux Window Resolver
 * Resolves the correct tmux window for a Claude hook process
 *
 * Chain: Hook Process → Parent PID → TTY → TMux Window
 */

const { execSync } = require('child_process');

function getParentTTY(ppid) {
    try {
        // Get the TTY of the parent process
        const tty = execSync(`ps -o tty= -p ${ppid} 2>/dev/null`, {
            encoding: 'utf8'
        }).trim();

        if (tty && tty !== '?' && tty !== '??') {
            // Add /dev/ prefix if not present
            return tty.startsWith('/dev/') ? tty : `/dev/${tty}`;
        }
    } catch (e) {}
    return null;
}

function findTmuxWindowByTTY(tty) {
    if (!tty) return null;

    try {
        // List all tmux panes with their TTY and window index
        const panes = execSync(
            'tmux list-panes -a -F "#{window_index}:#{pane_tty}"',
            { encoding: 'utf8' }
        ).trim().split('\n');

        for (const pane of panes) {
            const [windowId, paneTty] = pane.split(':');
            if (paneTty === tty) {
                return windowId;
            }
        }
    } catch (e) {}
    return null;
}

function getClaudeProcessInfo(pid) {
    try {
        // Get command line for process to identify Claude processes
        const cmdline = execSync(`ps -o cmd= -p ${pid} 2>/dev/null`, {
            encoding: 'utf8'
        }).trim();

        return {
            pid,
            cmdline,
            isClaude: cmdline.includes('claude') || cmdline.includes('Claude')
        };
    } catch (e) {
        return { pid, cmdline: '', isClaude: false };
    }
}

function resolveWindow() {
    // First check if we have a cached window ID from environment
    if (process.env.CLAUDE_TMUX_WINDOW) {
        const cachedWindow = process.env.CLAUDE_TMUX_WINDOW;
        require('fs').appendFileSync('/tmp/tmux-resolution.log',
            `Env Resolution: Using cached window ${cachedWindow}\n`);
        return cachedWindow;
    }

    // Check if we're in tmux at all
    if (!process.env.TMUX) {
        return null;
    }

    // DO NOT use active window - it returns what the user is looking at,
    // not where Claude is running! Must use TTY-based resolution.

    // Get the parent process ID (Claude process)
    let currentPid = process.ppid;
    let attempts = 0;
    const maxAttempts = 7; // Increased to go deeper in process tree
    const processTrace = []; // Track the process traversal

    while (currentPid && attempts < maxAttempts) {
        // Get process info for debugging
        const processInfo = getClaudeProcessInfo(currentPid);
        processTrace.push(`${currentPid}(${processInfo.isClaude ? 'CLAUDE' : 'other'})`);

        // Get the TTY of the current process
        const tty = getParentTTY(currentPid);

        if (tty) {
            // Try to find tmux window with this TTY
            const windowId = findTmuxWindowByTTY(tty);
            if (windowId) {
                // Cache for future use
                process.env.CLAUDE_TMUX_WINDOW = windowId;
                // Log successful resolution with process trace
                const traceInfo = `TTY Resolution: ${processTrace.join(' -> ')} -> TTY:${tty} -> Window:${windowId}\n`;
                require('fs').appendFileSync('/tmp/tmux-resolution.log', traceInfo);
                return windowId;
            }
        }

        // Go up one level in the process tree
        try {
            const parentPid = execSync(`ps -o ppid= -p ${currentPid} 2>/dev/null`, {
                encoding: 'utf8'
            }).trim();

            if (!parentPid || parentPid === '1' || parentPid === currentPid) {
                break; // Reached init process or circular reference
            }

            currentPid = parentPid;
            attempts++;
        } catch (e) {
            break;
        }
    }

    // Log failed resolution
    const failInfo = `TTY Resolution FAILED: ${processTrace.join(' -> ')} -> NO_WINDOW\n`;
    require('fs').appendFileSync('/tmp/tmux-resolution.log', failInfo);

    return null; // Could not find tmux window
}

// REMOVED: updateTmuxWindow function
// Window updates should ONLY go through tmux-status-coordinator.js
// Use: const { updateTmuxWindow } = require('./tmux-status-coordinator');

module.exports = {
    resolveWindow,
    // updateTmuxWindow removed - use tmux-status-coordinator instead
    getParentTTY,
    findTmuxWindowByTTY
};