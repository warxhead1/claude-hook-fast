#!/usr/bin/env node

/**
 * TMux Ready Status Hook - Triggered when Claude finishes responding
 * Shows session statistics
 */

const { execSync } = require('child_process');
const fs = require('fs');
const { updateTmuxWindow } = require('../lib/tmux-status-coordinator');
const { resolveWindow } = require('../lib/tmux-window-resolver');
const { StatusHistory } = require('../lib/status-history');
const { sessionCleanup } = require('../lib/session-cleanup');
const { endWorkSession } = require('../lib/work-session');
const { processRegistry } = require('../lib/process-registry');

function main() {
    try {
        if (!process.env.TMUX) return;

        // Get the window ID first so we can use window-specific stats
        const { resolveWindow } = require('../lib/tmux-window-resolver');
        const windowId = resolveWindow();

        // 🚨 STOP HOOK FORENSICS - Log detailed information about what triggered this
        const triggerInfo = {
            timestamp: new Date().toISOString(),
            pid: process.pid,
            ppid: process.ppid,
            windowId: windowId,
            env_vars: {
                TMUX: process.env.TMUX,
                PWD: process.env.PWD,
                DISPLAY: process.env.DISPLAY
            },
            process_info: {
                argv: process.argv,
                cwd: process.cwd(),
                uid: process.getuid ? process.getuid() : 'unknown',
                gid: process.getgid ? process.getgid() : 'unknown'
            }
        };

        // Capture parent process info to understand what triggered the stop
        try {
            const { execSync } = require('child_process');
            const psInfo = execSync(`ps -p ${process.ppid} -o pid,ppid,command --no-headers`, { encoding: 'utf8', timeout: 1000 }).trim();
            triggerInfo.parent_process = psInfo;

            // Get the full process tree to see the chain
            const pstreeInfo = execSync(`pstree -p ${process.ppid}`, { encoding: 'utf8', timeout: 1000 }).trim();
            triggerInfo.process_tree = pstreeInfo;
        } catch (e) {
            triggerInfo.parent_process_error = e.message;
        }

        // Check for recent signals or process events
        try {
            // Look for recent process termination events
            const dmesgRecent = execSync(`dmesg | tail -10 | grep -E "(signal|kill|term|exit)"`, { encoding: 'utf8', timeout: 1000 }).trim();
            if (dmesgRecent) {
                triggerInfo.recent_signals = dmesgRecent;
            }
        } catch (e) {
            // No recent signals found, which is normal
        }

        // Get session/conversation context from stdin if available
        try {
            if (!process.stdin.isTTY) {
                let input = '';
                const chunks = [];
                // Use a quick timeout to avoid blocking
                const timeout = setTimeout(() => {
                    process.stdin.destroy();
                }, 100);

                process.stdin.on('data', chunk => chunks.push(chunk));
                process.stdin.on('end', () => {
                    clearTimeout(timeout);
                    input = Buffer.concat(chunks).toString();
                    if (input.trim()) {
                        try {
                            const hookData = JSON.parse(input);
                            triggerInfo.hook_context = {
                                session_id: hookData.session_id,
                                conversation_id: hookData.conversation_id,
                                transcript_path: hookData.transcript_path,
                                tool_name: hookData.tool_name,
                                permission_mode: hookData.permission_mode
                            };
                        } catch (e) {
                            triggerInfo.hook_context_raw = input.substring(0, 200); // First 200 chars
                        }
                    }
                });
            }
        } catch (e) {
            triggerInfo.stdin_error = e.message;
        }

        // Log the comprehensive trigger information
        fs.appendFileSync('/tmp/stop-hook-forensics.log', JSON.stringify(triggerInfo, null, 2) + '\n---\n');
        fs.appendFileSync('/tmp/tmux-hook.log', `${new Date().toISOString()} - STOP HOOK TRIGGERED: windowId=${windowId} PID=${process.pid} PPID=${process.ppid} (see /tmp/stop-hook-forensics.log for details)\n`);

        const history = new StatusHistory(windowId);

        // 🎯 END WORK SESSION - This is the definitive signal that Claude is done working
        endWorkSession('stop_hook', windowId);

        // 🏷️ DETERMINE CLIENT TYPE for appropriate naming
        let clientType = 'Claude'; // Default
        try {
            const parentPid = process.ppid;
            const detectedType = processRegistry.detectProcessType(parentPid);
            clientType = processRegistry.getClientTypeLabel(detectedType);
        } catch (e) {
            // Fall back to default if detection fails
        }

        // Build client-aware ready status
        let statusText = `✓ ${clientType} ready`;

        // 🧹 COMPREHENSIVE CLEANUP: Clean up any lingering technical state
        // The Stop hook may not have session_id, so use window-based cleanup
        // This just cleans technical debris from tools that just completed
        sessionCleanup.cleanupWindow(windowId, 'stop-hook-cleanup', false);

        // 🧹 ADDITIONAL CLEANUP: Clean up stale state from other sessions
        cleanupStaleState(windowId);

        // 🚨 FORCE CLEAR any stuck status (like "Waiting for input")
        // The Stop hook should ALWAYS override any stuck status when Claude finishes

        // Always use the coordinator for window updates
        updateTmuxWindow(statusText, windowId, 'stop-hook');
        fs.appendFileSync('/tmp/tmux-hook.log',
            `${new Date().toISOString()} - Stop Hook: Updated window ${windowId || 'current'} to "${statusText}" (client: ${clientType})\n`);

        // Double-tap to ensure it overrides any persistent status
        setTimeout(() => {
            updateTmuxWindow(statusText, windowId, 'stop-hook-double-tap');
        }, 50);

        // Log for debugging
        const debugInfo = `${new Date().toISOString()} - Stop Hook: Cleared alerts and set ready [Window:${windowId}]\n`;
        fs.appendFileSync('/tmp/tmux-hook.log', debugInfo);
    } catch (error) {
        // Silent fail
    }
}

function cleanupStaleState(currentWindowId) {
    try {
        // 🧹 1. Clean up old approval alerts from other sessions
        const { alerter } = require('../lib/approval-alerter-simple');
        const alerts = alerter.loadAlerts();
        const now = Date.now();
        const maxAge = 10 * 60 * 1000; // 10 minutes
        let cleaned = 0;

        for (const [windowId, alert] of Object.entries(alerts)) {
            if (now - alert.startTime > maxAge) {
                delete alerts[windowId];
                cleaned++;
            }
        }

        if (cleaned > 0) {
            alerter.saveAlerts(alerts);
            fs.appendFileSync('/tmp/tmux-hook.log', `${new Date().toISOString()} - Cleanup: Removed ${cleaned} stale approval alerts\n`);
        }

        // 🧹 2. Clean up old timing data
        const { timingTracker } = require('../lib/timing-tracker');
        timingTracker.cleanup();

        // 🧹 3. Rotate log files if they get too large
        try {
            const logFile = '/tmp/tmux-hook.log';
            const stats = fs.statSync(logFile);
            const maxLogSize = 5 * 1024 * 1024; // 5MB

            if (stats.size > maxLogSize) {
                // Keep last 1000 lines
                const content = fs.readFileSync(logFile, 'utf8');
                const lines = content.split('\n');
                const recentLines = lines.slice(-1000);
                fs.writeFileSync(logFile, recentLines.join('\n') + '\n');
                fs.appendFileSync('/tmp/tmux-hook.log', `${new Date().toISOString()} - Cleanup: Log file rotated (${lines.length} -> ${recentLines.length} lines)\n`);
            }
        } catch (e) {}

        // 🧹 4. Clean up orphaned daemon processes (simple check)
        try {
            const { execSync } = require('child_process');
            // Count alerter-daemon processes - there should be 0-2 normally
            const daemonCount = execSync('pgrep -f alerter-daemon.js | wc -l', { encoding: 'utf8' }).trim();
            if (parseInt(daemonCount) > 5) {
                fs.appendFileSync('/tmp/tmux-hook.log', `${new Date().toISOString()} - Cleanup: Warning - ${daemonCount} daemon processes detected\n`);
            }
        } catch (e) {}

        // 🧹 5. Clean up session statistics files older than 24 hours
        try {
            const statsPattern = '/tmp/tmux-session-stats-*.json';
            const { execSync } = require('child_process');
            execSync(`find /tmp -name "tmux-session-stats-*.json" -mtime +1 -delete`, { stdio: 'ignore' });
        } catch (e) {}

    } catch (error) {
        // Silent fail - cleanup should never break the main flow
        fs.appendFileSync('/tmp/tmux-hook.log', `${new Date().toISOString()} - Cleanup error: ${error.message}\n`);
    }
}

main();