#!/usr/bin/env node

/**
 * TMux Notification Hook - Shows when Claude needs approval or is waiting
 */

const { execSync } = require('child_process');
const fs = require('fs');
const { alerter } = require('../lib/approval-alerter-simple');
const { formatStatus } = require('../lib/status-formatter');
const {
    updateApprovalPending,
    updateApprovalDenied,
    updateNotification
} = require('../lib/tmux-status-coordinator');
const { getWindowId } = require('../lib/session-manager');

function main() {
    try {
        if (!process.env.TMUX) return;

        const input = JSON.parse(fs.readFileSync(0, 'utf8'));
        const windowId = getWindowId();

        // Log notification events with full input for debugging
        const debugInfo = `${new Date().toISOString()} - Notification [PID:${process.pid} Window:${windowId}]: ${JSON.stringify(input)}\n`;
        fs.appendFileSync('/tmp/tmux-hook.log', debugInfo);

        // Check if this is an approval notification based on the message content
        const message = input.message || '';

        if (message.includes('permission') || message.includes('approval')) {
            // Extract tool name from message if possible
            // Examples: "Claude needs your permission to use Read"
            //           "Claude Code needs your approval for the plan"
            let toolName = 'tool';
            const toolMatch = message.match(/permission to use (\w+)/i);
            if (toolMatch) {
                toolName = toolMatch[1];
            } else if (message.includes('approval for the plan')) {
                toolName = 'Plan';
            }

            // Use coordinator for approval pending status
            updateApprovalPending(toolName);

            // Start the approval alerter with animations and flashing
            const approvalText = formatStatus(`${toolName} approval`, 'info');
            alerter.startApprovalAlert(windowId, toolName, approvalText);
        } else if (message.includes('waiting for your input')) {
            // This happens after denial or "other" option - stop any approval alerts
            alerter.stopAlert(windowId);

            // 🔍 DEBUG: Log when we set "Waiting for input" and why
            fs.appendFileSync('/tmp/tmux-hook.log', `${new Date().toISOString()} - Notification: Setting "Waiting for input" on window ${windowId}, message: "${message}"\n`);

            updateNotification("waiting for input");
        } else {
            // Other notification types
            updateNotification("notification");
        }
    } catch (error) {
        // Silent fail
    }
}

main();