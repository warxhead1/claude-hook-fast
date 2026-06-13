#!/usr/bin/env node

/**
 * Tmux Status Coordinator - Centralized tmux window status management
 *
 * Features:
 * - Priority-based status updates (approval alerts > tool activity > achievements)
 * - Prevents status conflicts between hooks
 * - Session-aware status management
 * - Status history with automatic cleanup
 * - Intelligent status timing and persistence
 */

const { execSync } = require('child_process');
const fs = require('fs');
const { getSessionManager, getSessionId, getWindowId } = require('./session-manager');
const { formatStatus } = require('./status-formatter');
const { getFocusName } = require('./focus-namer');

// Status priority levels (higher number = higher priority)
const PRIORITY = {
    IDLE: 0,
    ACHIEVEMENT: 10,
    TOOL_PREPARING: 20,
    TOOL_WORKING: 30,
    TOOL_SUCCESS: 25,
    TOOL_ERROR: 35,
    APPROVAL_PENDING: 90,
    APPROVAL_DENIED: 95,
    SYSTEM_ERROR: 100
};

// Status persistence times (how long each type should stay visible)
const PERSISTENCE = {
    [PRIORITY.IDLE]: 0,
    [PRIORITY.ACHIEVEMENT]: 5000,      // 5 seconds
    [PRIORITY.TOOL_PREPARING]: 0,      // Immediate replacement
    [PRIORITY.TOOL_WORKING]: 0,        // Immediate replacement
    [PRIORITY.TOOL_SUCCESS]: 3000,     // 3 seconds
    [PRIORITY.TOOL_ERROR]: 10000,      // 10 seconds
    [PRIORITY.APPROVAL_PENDING]: 0,    // Until resolved
    [PRIORITY.APPROVAL_DENIED]: 8000,  // 8 seconds
    [PRIORITY.SYSTEM_ERROR]: 15000     // 15 seconds
};

class TmuxStatusCoordinator {
    constructor() {
        this.sessionManager = getSessionManager();
        this.statusFile = this.sessionManager.getSessionFile('status-coordinator');
        this.currentStatus = this.loadCurrentStatus();
        this.pendingTimeouts = new Map(); // Window-specific timeouts
    }

    /**
     * Load current status state
     */
    loadCurrentStatus() {
        try {
            if (fs.existsSync(this.statusFile)) {
                return JSON.parse(fs.readFileSync(this.statusFile, 'utf8'));
            }
        } catch (e) {
            this.logError('loadCurrentStatus', e);
        }

        return {
            text: '',
            priority: PRIORITY.IDLE,
            timestamp: Date.now(),
            sessionId: getSessionId(),
            windowId: getWindowId(),
            source: 'init'
        };
    }

    /**
     * Save current status state
     */
    saveCurrentStatus() {
        try {
            fs.writeFileSync(this.statusFile, JSON.stringify(this.currentStatus, null, 2));
        } catch (e) {
            this.logError('saveCurrentStatus', e);
        }
    }

    /**
     * Update tmux window status with priority checking
     */
    updateStatus(text, priority, source = 'unknown', options = {}) {
        const windowId = getWindowId();
        const now = Date.now();
        const sessionId = getSessionId();

        // Calculate if this update should be allowed
        const shouldUpdate = this.shouldUpdateStatus(priority, now, source);

        if (!shouldUpdate) {
            this.logBlocked(text, priority, source, this.currentStatus);
            return false;
        }

        // Clear any pending timeout for this window
        this.clearPendingTimeout(windowId);

        // Prepare the new status
        const newStatus = {
            text,
            priority,
            timestamp: now,
            sessionId,
            windowId,
            source,
            options
        };

        // Execute tmux update
        const success = this.executeTmuxUpdate(text, windowId, priority);

        if (success) {
            this.currentStatus = newStatus;
            this.saveCurrentStatus();

            // Set automatic revert timer if needed
            this.scheduleRevert(priority, windowId);

            this.logUpdate(newStatus);
            return true;
        }

        return false;
    }

    /**
     * Determine if status should be updated based on priority and timing
     */
    shouldUpdateStatus(newPriority, now, source) {
        const current = this.currentStatus;

        // Always allow if no current status
        if (!current || current.priority === PRIORITY.IDLE) {
            return true;
        }

        // Higher priority always wins
        if (newPriority > current.priority) {
            return true;
        }

        // Same priority: check timing
        if (newPriority === current.priority) {
            // Allow updates from same source type
            if (this.isSameSourceType(source, current.source)) {
                return true;
            }
            // Allow if current status is stale
            const timeSinceUpdate = now - current.timestamp;
            const persistence = PERSISTENCE[current.priority] || 3000;
            return timeSinceUpdate > persistence;
        }

        // Lower priority: only if current status has expired
        const timeSinceUpdate = now - current.timestamp;
        const persistence = PERSISTENCE[current.priority] || 3000;

        return persistence > 0 && timeSinceUpdate > persistence;
    }

    /**
     * Check if two sources are the same type (for same-priority replacement)
     */
    isSameSourceType(source1, source2) {
        const types = {
            'tmux-preparing': 'tool',
            'tmux-status': 'tool',
            'approval-alerter': 'approval',
            'notification': 'system',
            'achievement': 'achievement'
        };

        const type1 = types[source1] || 'other';
        const type2 = types[source2] || 'other';

        return type1 === type2;
    }

    /**
     * Execute the actual tmux window rename.
     * Format: "{focus} {icon}" — short, consistent, meaningful.
     * Approval overrides everything: "⚠ APPROVE"
     */
    executeTmuxUpdate(text, windowId, priority) {
        try {
            let displayText;

            // Approval states scream — no focus, just the alert
            const isApproval = (priority >= PRIORITY.APPROVAL_PENDING) ||
                text.includes('APPROV') || text.includes('🚨');

            if (isApproval) {
                displayText = '⚠ APPROVE';
            } else {
                // Extract state icon from the text
                let icon = '⚡'; // default: working
                if (text.startsWith('✓') || text.includes('ready') || text.includes('Ready')) icon = '✓';
                else if (text.startsWith('✗') || text.includes('error') || text.includes('failed') || text.includes('denied')) icon = '✗';

                // Get focus name (branch-first, file fallback)
                let focus;
                try { focus = getFocusName(); } catch {}
                focus = focus || 'claude';

                displayText = `${focus} ${icon}`;
            }

            // Escape text for shell safety
            const escapedText = displayText.replace(/"/g, '\\"').replace(/'/g, "\\'");

            if (windowId && windowId !== 'current') {
                // Get current session name for precise targeting
                const sessionName = process.env.TMUX ?
                    execSync('tmux display-message -p "#{session_name}"', { encoding: 'utf8' }).trim() :
                    null;

                const target = sessionName ? `${sessionName}:${windowId}` : windowId;
                execSync(`tmux rename-window -t ${target} "${escapedText}"`, {
                    stdio: 'ignore',
                    timeout: 3000
                });
            } else {
                execSync(`tmux rename-window "${escapedText}"`, {
                    stdio: 'ignore',
                    timeout: 3000
                });
            }

            return true;
        } catch (error) {
            this.logError('executeTmuxUpdate', error, { text, windowId });
            return false;
        }
    }

    /**
     * Schedule automatic revert to lower priority status
     */
    scheduleRevert(priority, windowId) {
        const persistence = PERSISTENCE[priority];

        if (persistence > 0) {
            // Clear any existing timeout
            this.clearPendingTimeout(windowId);

            // Set new timeout
            const timeoutId = setTimeout(() => {
                this.revertToIdle(windowId);
            }, persistence);

            this.pendingTimeouts.set(windowId, timeoutId);
        }
    }

    /**
     * Clear pending timeout for a window
     */
    clearPendingTimeout(windowId) {
        if (this.pendingTimeouts.has(windowId)) {
            clearTimeout(this.pendingTimeouts.get(windowId));
            this.pendingTimeouts.delete(windowId);
        }
    }

    /**
     * Revert status to idle state - WITH WORK SESSION AWARENESS
     */
    revertToIdle(windowId) {
        this.clearPendingTimeout(windowId);

        // Only revert if we're still the active status
        if (this.currentStatus && this.currentStatus.windowId === windowId) {
            // CHECK: Is there an active work session?
            const { isWorkSessionActive, getWorkSessionStatus } = require('./work-session');

            if (isWorkSessionActive(windowId)) {
                // DON'T show "ready" - we're still working on something!
                // Instead, show work session status or keep current tool status
                const workStatus = getWorkSessionStatus(windowId);
                if (workStatus) {
                    try {
                        this.updateStatus(
                            workStatus,
                            PRIORITY.TOOL_WORKING,
                            'work-session-continue'
                        );
                    } catch (e) {
                        // Fallback: just don't change status during work session
                    }
                }
                // If no work status available, keep current status (don't revert)
                return;
            }

            // No active work session - NOW we can show ready
            try {
                this.updateStatus(
                    '✓ ready',
                    PRIORITY.IDLE,
                    'auto-revert'
                );
            } catch (e) {
                // If we can't update, just clear the status
                this.currentStatus = null;
            }
        }
    }

    /**
     * Force clear status (for emergency situations)
     */
    forceClear(windowId, reason = 'manual') {
        this.clearPendingTimeout(windowId);

        this.currentStatus = {
            text: formatStatus('cleared', 'info'), // Use consistent Status: format
            priority: PRIORITY.IDLE,
            timestamp: Date.now(),
            sessionId: getSessionId(),
            windowId: windowId,
            source: 'force-clear',
            reason
        };

        this.executeTmuxUpdate(this.currentStatus.text, windowId, PRIORITY.IDLE);
        this.saveCurrentStatus();

        this.logUpdate(this.currentStatus);
    }

    /**
     * Get current status information
     */
    getCurrentStatus() {
        return { ...this.currentStatus };
    }

    /**
     * Check if a specific priority level can update
     */
    canUpdate(priority) {
        return this.shouldUpdateStatus(priority, Date.now(), 'check');
    }

    /**
     * Log successful status update
     */
    logUpdate(status) {
        try {
            const logEntry = {
                timestamp: new Date().toISOString(),
                type: 'status_update',
                status,
                sessionId: getSessionId()
            };

            fs.appendFileSync('/tmp/tmux-coordinator.log',
                JSON.stringify(logEntry) + '\n');
        } catch (e) {
            // Silent fail for logging
        }
    }

    /**
     * Log blocked status update
     */
    logBlocked(text, priority, source, currentStatus) {
        try {
            const logEntry = {
                timestamp: new Date().toISOString(),
                type: 'status_blocked',
                attempted: { text, priority, source },
                current: currentStatus,
                sessionId: getSessionId()
            };

            fs.appendFileSync('/tmp/tmux-coordinator.log',
                JSON.stringify(logEntry) + '\n');
        } catch (e) {
            // Silent fail for logging
        }
    }

    /**
     * Log errors
     */
    logError(operation, error, context = {}) {
        try {
            const logEntry = {
                timestamp: new Date().toISOString(),
                type: 'error',
                operation,
                error: error.message,
                context,
                sessionId: getSessionId()
            };

            fs.appendFileSync('/tmp/tmux-coordinator.log',
                JSON.stringify(logEntry) + '\n');
        } catch (e) {
            // Silent fail for logging
        }
    }
}

// Process-isolated coordinator instances keyed by sessionId
const coordinatorInstances = new Map();

/**
 * Get the process-isolated coordinator instance
 * Each hook process gets its own coordinator based on sessionId,
 * preventing window status updates from interfering across sessions
 */
function getStatusCoordinator() {
    const sessionId = getSessionId();

    if (!coordinatorInstances.has(sessionId)) {
        // Periodic cleanup of stale coordinators (every 10th access)
        if (Math.random() < 0.1) {
            cleanupStaleCoordinators();
        }

        coordinatorInstances.set(sessionId, new TmuxStatusCoordinator());
    }

    return coordinatorInstances.get(sessionId);
}

/**
 * Clear coordinator instance for a specific session (for cleanup)
 */
function clearStatusCoordinator(sessionId = null) {
    const targetSessionId = sessionId || getSessionId();

    if (coordinatorInstances.has(targetSessionId)) {
        const coordinator = coordinatorInstances.get(targetSessionId);

        // Clear all pending timeouts
        coordinator.pendingTimeouts.forEach(timeoutId => clearTimeout(timeoutId));
        coordinator.pendingTimeouts.clear();

        coordinatorInstances.delete(targetSessionId);
    }
}

/**
 * Cleanup stale coordinators for windows that no longer exist
 * Should be called periodically to prevent memory leaks
 */
function cleanupStaleCoordinators() {
    try {
        // Get all active tmux windows
        const activeWindows = new Set();
        const sessions = execSync('tmux list-sessions -F "#{session_name}"', {
            encoding: 'utf8',
            timeout: 2000
        }).trim().split('\n');

        for (const session of sessions) {
            const windows = execSync(`tmux list-windows -t "${session}" -F "#{session_name}-#{window_index}-#{pane_index}"`, {
                encoding: 'utf8',
                timeout: 2000
            }).trim().split('\n');

            windows.forEach(window => activeWindows.add(window));
        }

        // Remove coordinators for sessions that no longer exist
        for (const [sessionId, coordinator] of coordinatorInstances.entries()) {
            if (!activeWindows.has(sessionId)) {
                // Clear timeouts and remove stale coordinator
                coordinator.pendingTimeouts.forEach(timeoutId => clearTimeout(timeoutId));
                coordinator.pendingTimeouts.clear();
                coordinatorInstances.delete(sessionId);

                // Also cleanup the status file
                try {
                    const fs = require('fs');
                    const statusFile = `/tmp/claude-status-coordinator-${sessionId.replace(/[^a-zA-Z0-9-_]/g, '_')}.json`;
                    if (fs.existsSync(statusFile)) {
                        fs.unlinkSync(statusFile);
                    }
                } catch (e) {
                    // Silent cleanup failure
                }
            }
        }
    } catch (e) {
        // Silent fail - cleanup is best effort
    }
}

/**
 * Convenience functions for common status updates
 */
function updateToolPreparing(toolName, toolInput) {
    const coordinator = getStatusCoordinator();
    const { formatToolStatus } = require('./status-formatter');

    // formatToolStatus already includes formatting, just pass the raw text
    const rawText = formatToolStatus(toolName, toolInput, true);
    const statusText = formatStatus(rawText, 'working', { usePrefix: false });

    return coordinator.updateStatus(statusText, PRIORITY.TOOL_PREPARING, 'tmux-preparing');
}

function updateToolWorking(toolName, toolInput) {
    const coordinator = getStatusCoordinator();
    const { formatToolStatus } = require('./status-formatter');

    const statusText = formatStatus(
        formatToolStatus(toolName, toolInput, true),
        'working'
    );

    return coordinator.updateStatus(statusText, PRIORITY.TOOL_WORKING, 'tmux-status');
}

function updateToolSuccess(toolName, toolInput, enhancedStatus = null) {
    const coordinator = getStatusCoordinator();
    const { formatToolStatus } = require('./status-formatter');

    // CHECK: Are we in an active work session?
    const { isWorkSessionActive, getWorkSessionStatus } = require('./work-session');
    const windowId = require('./session-manager').getWindowId();

    if (isWorkSessionActive(windowId)) {
        // During work session: immediately show generic work status instead of tool-specific success
        const workStatus = getWorkSessionStatus(windowId);
        if (workStatus) {
            return coordinator.updateStatus(workStatus, PRIORITY.TOOL_WORKING, 'work-session-continue');
        } else {
            // Fallback generic work status
            return coordinator.updateStatus('⚡ working...', PRIORITY.TOOL_WORKING, 'work-session-generic');
        }
    } else {
        // No work session: show normal tool success status
        const statusText = enhancedStatus || formatStatus(
            formatToolStatus(toolName, toolInput, true),
            'success'
        );

        return coordinator.updateStatus(statusText, PRIORITY.TOOL_SUCCESS, 'tmux-status');
    }
}

function updateToolError(toolName, toolInput, errorMessage) {
    const coordinator = getStatusCoordinator();
    const { formatToolStatus } = require('./status-formatter');

    const statusText = formatStatus(
        formatToolStatus(toolName, toolInput, false, errorMessage),
        'error'
    );

    return coordinator.updateStatus(statusText, PRIORITY.TOOL_ERROR, 'tmux-status');
}

function updateApprovalPending(toolName) {
    const coordinator = getStatusCoordinator();
    const statusText = formatStatus(`${toolName} approval`, 'info');

    return coordinator.updateStatus(statusText, PRIORITY.APPROVAL_PENDING, 'approval-alerter');
}

function updateApprovalDenied(toolName) {
    const coordinator = getStatusCoordinator();
    const statusText = formatStatus(`${toolName} denied`, 'error');

    return coordinator.updateStatus(statusText, PRIORITY.APPROVAL_DENIED, 'approval-alerter');
}

function updateAchievement(achievementText) {
    const coordinator = getStatusCoordinator();
    const statusText = formatStatus(achievementText, 'success');

    return coordinator.updateStatus(statusText, PRIORITY.ACHIEVEMENT, 'achievement');
}

function updateNotification(notificationText) {
    const coordinator = getStatusCoordinator();
    const statusText = formatStatus(notificationText, 'info');

    return coordinator.updateStatus(statusText, PRIORITY.TOOL_PREPARING, 'notification');
}

function clearApprovalWithWorkSessionAwareness(windowId) {
    const coordinator = getStatusCoordinator();

    // Check if there's an active work session
    const { isWorkSessionActive, getWorkSessionStatus } = require('./work-session');

    if (isWorkSessionActive(windowId)) {
        // Resume work session status instead of going to "ready"
        const workStatus = getWorkSessionStatus(windowId);
        if (workStatus) {
            coordinator.updateStatus(workStatus, PRIORITY.TOOL_WORKING, 'approval-cleared-resume-work');
        } else {
            // Fallback: generic working status
            coordinator.updateStatus('⚡ working...', PRIORITY.TOOL_WORKING, 'approval-cleared-working');
        }
    } else {
        // No active work session - go to ready
        coordinator.updateStatus('✓ ready', PRIORITY.IDLE, 'approval-cleared-ready');
    }
}

function clearStatus(reason = 'manual') {
    const coordinator = getStatusCoordinator();
    coordinator.forceClear(getWindowId(), reason);
}

function getCurrentStatus() {
    const coordinator = getStatusCoordinator();
    return coordinator.getCurrentStatus();
}

function canUpdate(priority) {
    const coordinator = getStatusCoordinator();
    return coordinator.canUpdate(priority);
}

/**
 * Direct tmux window update - THE ONLY FUNCTION that should rename windows
 * @param {string} text - Text to display
 * @param {string} windowId - Optional window ID (uses current if not provided)
 * @param {string} source - Source of the update for logging
 */
function updateTmuxWindow(text, windowId = null, source = 'direct') {
    const coordinator = getStatusCoordinator();

    // If no window ID provided, try to resolve it
    if (!windowId) {
        const { resolveWindow } = require('./tmux-window-resolver');
        windowId = resolveWindow();
    }

    // Log the update attempt
    const fs = require('fs');
    fs.appendFileSync('/tmp/tmux-hook.log',
        `${new Date().toISOString()} - updateTmuxWindow: source=${source}, window=${windowId}, text="${text}"\n`);

    // Use the coordinator's internal method
    return coordinator.executeTmuxUpdate(text, windowId);
}

module.exports = {
    TmuxStatusCoordinator,
    getStatusCoordinator,
    clearStatusCoordinator,
    cleanupStaleCoordinators,
    PRIORITY,
    PERSISTENCE,

    // Convenience functions
    updateToolPreparing,
    updateToolWorking,
    updateToolSuccess,
    updateToolError,
    updateApprovalPending,
    updateApprovalDenied,
    updateAchievement,
    updateNotification,
    clearStatus,
    getCurrentStatus,
    canUpdate,

    // Work session aware functions
    clearApprovalWithWorkSessionAwareness,

    // Direct update function - use this instead of calling tmux directly!
    updateTmuxWindow
};