#!/usr/bin/env node

/**
 * TMux Pre-Tool Hook - Shows when Claude is about to use a tool
 * Shows actual command/file context while preparing
 */

const startTime = Date.now();
const timings = {};
function mark(name) { timings[name] = Date.now() - startTime; }

const { execSync } = require('child_process');
const fs = require('fs');
mark('require_core');
const { StatusHistory } = require('../lib/status-history');
mark('require_status_history');
mark('require_spinner');
const { alerter } = require('../lib/approval-alerter-simple');
mark('require_alerter');
const { contextDetector } = require('../lib/context-detector');
mark('require_context_detector');
const { timingTracker } = require('../lib/timing-tracker');
mark('require_timing_tracker');
const { formatStatus, formatToolStatus, formatContextualStatus, gatherWorkflowContext } = require('../lib/status-formatter');
mark('require_status_formatter');
const { updateToolPreparing } = require('../lib/tmux-status-coordinator');
mark('require_tmux_coordinator');
const { getWindowId } = require('../lib/session-manager');
mark('require_session_manager');
const { sessionCleanup } = require('../lib/session-cleanup');
mark('require_session_cleanup');
const { conversationTracker } = require('../lib/conversation-tracker');
mark('require_conversation_tracker');
mark('require_daemon_manager');
const { startWorkSession } = require('../lib/work-session');
mark('require_work_session');
const { processRegistry } = require('../lib/process-registry');
mark('require_process_registry');

// Helper functions now handled by status-formatter.js

function main() {
    try {
        if (!process.env.TMUX) return;

        // Read stdin - this will block if no data is provided
        let input;
        try {
            const stdinBuffer = fs.readFileSync(0, 'utf8');
            input = JSON.parse(stdinBuffer);
        } catch (e) {
            // Silent fail if we can't parse
            return;
        }

        const toolName = input.tool_name;
        const toolInput = input.tool_input || {};
        const requiresApproval = input.requires_approval || input.requires_permission || false;
        const sessionId = input.session_id;
        const transcriptPath = input.transcript_path;
        mark('parse_input');

        mark('daemon_check');

        // Record operation for context analysis
        contextDetector.recordOperation(toolName, toolInput, 'preparing');
        mark('context_record');

        // ENHANCED CONTEXTUAL TITLE GENERATION for preparing state
        const workflowContext = gatherWorkflowContext();
        mark('workflow_context');
        workflowContext.intensity = 'medium'; // Default for preparing state
        workflowContext.workflowPhase = 'focused'; // Default phase for preparation

        // Generate contextual status with "preparing" indicator
        let statusText = formatContextualStatus(toolName, toolInput, true, {
            ...workflowContext,
            workflowPhase: 'starting' // Use starting phase to get → indicator
        });
        mark('format_status');

        // Fallback to enhanced contextual status if available
        if (!statusText || statusText.length < 5) {
            statusText = contextDetector.getContextualStatus(toolName, toolInput,
                formatStatus(formatToolStatus(toolName, toolInput, true), 'working'));
        }
        mark('fallback_status');

        // Use coordinator for status updates with proper prioritization
        updateToolPreparing(toolName, toolInput);
        mark('update_preparing');

        // Get window ID for other operations
        const windowId = getWindowId();
        mark('get_window_id');

        // 🎯 START WORK SESSION - Track this as part of ongoing work
        try {
            // Detect client type for work session context
            let clientType = 'unknown';
            try {
                const parentPid = process.ppid;
                clientType = processRegistry.detectProcessType(parentPid);
            } catch (e) {
                // Use unknown if detection fails
            }

            startWorkSession(toolName, toolInput, {
                sessionId,
                conversationId: conversationTracker.extractConversationId(transcriptPath),
                clientType
            });
        } catch (e) {
            // Non-critical - continue if work session fails
        }
        mark('work_session');

        // 🧹 CONVERSATION-CENTRIC CLEANUP: Clean up lingering state using stable conversation tracking
        // Extract conversation ID from transcript path for proper session continuity
        const conversationId = conversationTracker.extractConversationId(transcriptPath);

        if (conversationId) {
            // Update conversation activity and clean technical debris
            conversationTracker.updateConversation(conversationId, windowId, sessionId, false);

            // Clean any stuck progress monitors, approval alerts, etc. for this window
            sessionCleanup.cleanupWindow(windowId, 'conversation-tool-prep', false);
        } else {
            // Fallback to session-based cleanup if conversation ID extraction fails
            if (sessionId) {
                sessionCleanup.cleanupSession(sessionId, 'pre-tool-cleanup-fallback');
            } else {
                sessionCleanup.cleanupWindow(windowId, 'pre-tool-cleanup-emergency', false);
            }
        }
        mark('cleanup');

        // Start timing this operation
        timingTracker.startOperation(toolName, toolInput, windowId);
        mark('timing_start');

        // Note: Approval alerts were already cleared by sessionCleanup above

        mark('spinner');

        // Record in history
        const history = new StatusHistory(windowId);
        history.recordOperation('pre', toolName, 'preparing', {
            windowId,
            statusText,
            command: toolInput.command,
            file: toolInput.file_path,
            pattern: toolInput.pattern
        });
        mark('history');

        // 🔧 AUTO-FIX: Bash commands with env vars at the start
        if (toolName === 'Bash' && toolInput.command) {
            const envVarPattern = /^([A-Z_][A-Z0-9_]*=[^\s]+(?:\s+[A-Z_][A-Z0-9_]*=[^\s]+)*)\s+(.+)$/;
            const match = toolInput.command.match(envVarPattern);

            if (match) {
                const envVars = match[1];
                const actualCommand = match[2];
                const fixedCommand = `env ${envVars} ${actualCommand}`;

                fs.appendFileSync('/tmp/tmux-hook.log',
                    `${new Date().toISOString()} - BashEnvFixer: ${toolInput.command} → ${fixedCommand}\n`);

                // Modify the input
                input.tool_input.command = fixedCommand;
            }
        }

    } catch (error) {
        // Silent fail
    }

    // Output proper JSON response for PreToolUse hook
    console.log(JSON.stringify({ decision: undefined }));
}

main();

// Log timing breakdown
const totalTime = Date.now() - startTime;
const timingStr = Object.entries(timings).map(([k,v]) => `${k}=${v}ms`).join(' ');
try {
    fs.appendFileSync('/tmp/tmux-hook.log',
        `${new Date().toISOString()} - TIMING-DETAIL: pre-tool-use total=${totalTime}ms ${timingStr}\n`);
} catch(e) {}