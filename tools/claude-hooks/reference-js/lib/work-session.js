#!/usr/bin/env node

/**
 * Work Session Manager - Tracks multi-tool work sessions to prevent premature "ready" status
 *
 * Key Functions:
 * - Detects when Claude/Codex starts working on a task (PreToolUse)
 * - Tracks tools used during the work session
 * - Only ends session when Stop hook fires (Claude done responding)
 * - Prevents status coordinator from showing "ready" between tool calls
 */

const fs = require('fs');
const path = require('path');
const { getSessionId, getWindowId } = require('./session-manager');

class WorkSessionManager {
    constructor() {
        this.sessionsFile = '/tmp/claude-work-sessions.json';
    }

    /**
     * Load active work sessions from file
     */
    loadSessions() {
        try {
            if (fs.existsSync(this.sessionsFile)) {
                return JSON.parse(fs.readFileSync(this.sessionsFile, 'utf8'));
            }
        } catch (e) {
            this.logError('loadSessions', e);
        }
        return {};
    }

    /**
     * Save work sessions to file
     */
    saveSessions(sessions) {
        try {
            fs.writeFileSync(this.sessionsFile, JSON.stringify(sessions, null, 2));
        } catch (e) {
            this.logError('saveSessions', e);
        }
    }

    /**
     * Start a new work session for a window
     */
    startSession(windowId, toolName, toolInput = {}, sessionContext = {}) {
        const sessions = this.loadSessions();
        const now = Date.now();

        // If there's already an active session for this window, continue it
        if (sessions[windowId] && sessions[windowId].active) {
            sessions[windowId].tools.push({
                name: toolName,
                input: toolInput,
                timestamp: now
            });
            sessions[windowId].lastActivity = now;
            sessions[windowId].toolCount = sessions[windowId].tools.length;
        } else {
            // Start new session
            sessions[windowId] = {
                active: true,
                startTime: now,
                lastActivity: now,
                toolCount: 1,
                sessionId: sessionContext.sessionId || getSessionId(),
                conversationId: sessionContext.conversationId,
                clientType: sessionContext.clientType,
                tools: [{
                    name: toolName,
                    input: toolInput,
                    timestamp: now
                }]
            };
        }

        this.saveSessions(sessions);

        this.logActivity(windowId, 'session_continue', {
            toolName,
            toolCount: sessions[windowId].toolCount,
            sessionAge: now - sessions[windowId].startTime
        });

        return sessions[windowId];
    }

    /**
     * Check if a work session is active for a window
     */
    isSessionActive(windowId) {
        const sessions = this.loadSessions();
        const session = sessions[windowId];

        if (!session || !session.active) {
            return false;
        }

        // Check if session is stale (older than 5 minutes for realistic idle detection)
        const now = Date.now();
        const maxAge = 5 * 60 * 1000; // 5 minutes - much more aggressive

        if (now - session.lastActivity > maxAge) {
            this.endSession(windowId, 'stale');
            return false;
        }

        return true;
    }

    /**
     * Check if a session is sleeping (inactive but resumable)
     */
    isSessionSleeping(windowId) {
        const sessions = this.loadSessions();
        const session = sessions[windowId];

        if (!session) {
            return false;
        }

        // Session exists but is not active (was ended by timeout, manual stop, etc.)
        if (!session.active && session.endReason === 'timeout') {
            // Check if it's recent enough to be considered "sleeping" vs completely dead
            const now = Date.now();
            const sleepAge = 24 * 60 * 60 * 1000; // 24 hours
            return (now - session.endTime) < sleepAge;
        }

        return false;
    }

    /**
     * Resume a sleeping session with new activity
     */
    resumeSession(windowId, toolName, toolInput = {}, sessionContext = {}) {
        const sessions = this.loadSessions();
        const session = sessions[windowId];

        if (session && this.isSessionSleeping(windowId)) {
            // Resume the session
            session.active = true;
            session.lastActivity = Date.now();
            session.resumedAt = Date.now();
            session.resumeCount = (session.resumeCount || 0) + 1;
            session.endReason = null;

            // Add the new tool
            session.tools.push({
                name: toolName,
                input: toolInput,
                timestamp: Date.now()
            });
            session.toolCount = session.tools.length;

            // Update session context if provided
            if (sessionContext.clientType) {
                session.clientType = sessionContext.clientType;
            }

            this.saveSessions(sessions);

            this.logActivity(windowId, 'session_resumed', {
                toolName,
                resumeCount: session.resumeCount,
                sleepDuration: Date.now() - session.endTime
            });

            return session;
        }

        // Not a sleeping session, start new one
        return this.startSession(windowId, toolName, toolInput, sessionContext);
    }

    /**
     * End a work session for a window
     */
    endSession(windowId, reason = 'stop_hook') {
        const sessions = this.loadSessions();
        const session = sessions[windowId];

        if (session && session.active) {
            session.active = false;
            session.endTime = Date.now();
            session.endReason = reason;

            this.logActivity(windowId, 'session_end', {
                reason,
                duration: session.endTime - session.startTime,
                toolCount: session.toolCount
            });
        }

        this.saveSessions(sessions);
    }

    /**
     * Get work session info for a window
     */
    getSessionInfo(windowId) {
        const sessions = this.loadSessions();
        return sessions[windowId] || null;
    }

    /**
     * Get formatted status for current work session
     */
    getWorkSessionStatus(windowId) {
        const session = this.getSessionInfo(windowId);

        if (!session || !session.active) {
            return null;
        }

        const toolCount = session.toolCount;
        const tools = session.tools;
        const lastTool = tools[tools.length - 1];

        // Determine client prefix
        let clientPrefix = '';
        if (session.clientType === 'codex') {
            clientPrefix = 'Codex: ';
        } else if (session.clientType === 'claude') {
            clientPrefix = 'Claude: ';
        }

        // Generate contextual workflow status with flavor - SIMPLIFIED
        if (toolCount === 1) {
            // First tool: show what we're doing
            const activity = this.getActivityFromTool(lastTool.name);
            return `⚡ ${clientPrefix}${activity}`;
        } else {
            // Multiple tools: show progression with workflow context
            const workflow = this.detectWorkflowProgression(tools);
            if (workflow) {
                return `⚡ ${clientPrefix}${workflow}`;
            } else {
                // Fallback: just show last activity
                const lastActivity = this.getActivityFromTool(lastTool.name);
                return `⚡ ${clientPrefix}${lastActivity}`;
            }
        }
    }

    /**
     * Convert tool name to readable activity
     */
    getActivityFromTool(toolName) {
        const activities = {
            'Read': 'reading',
            'Write': 'writing',
            'Edit': 'editing',
            'Bash': 'running',
            'Grep': 'searching',
            'Glob': 'finding',
            'MultiEdit': 'refactoring',
            'NotebookEdit': 'updating',
            'Task': 'processing',
            'WebFetch': 'fetching',
            'mcp__tengine-refactor__build': 'building',
            'mcp__tengine-refactor__search': 'searching',
            'mcp__tengine-refactor__analyze': 'analyzing'
        };

        // Handle MCP tools
        if (toolName.startsWith('mcp__')) {
            const parts = toolName.split('__');
            if (parts.length > 2) {
                const action = parts[2];
                return activities[`mcp__${parts[1]}__${action}`] || action;
            }
        }

        return activities[toolName] || toolName.toLowerCase();
    }

    /**
     * Detect workflow progression patterns to show meaningful status
     */
    detectWorkflowProgression(tools) {
        if (tools.length < 2) return null;

        const recentTools = tools.slice(-3).map(t => t.name); // Last 3 tools
        const activities = recentTools.map(tool => this.getActivityFromTool(tool));

        // Detect common patterns
        if (activities.includes('reading') && activities.includes('editing')) {
            return 'read → editing';
        }
        if (activities.includes('searching') && activities.includes('reading')) {
            return 'found → reading';
        }
        if (activities.includes('building') && activities.includes('running')) {
            return 'built → testing';
        }
        if (activities.includes('editing') && activities.includes('building')) {
            return 'edited → building';
        }
        if (activities.includes('analyzing') && activities.includes('editing')) {
            return 'analyzed → fixing';
        }
        if (activities.includes('searching') && activities.includes('analyzing')) {
            return 'found → analyzing';
        }

        // Generic progression indicators
        const lastActivity = activities[activities.length - 1];
        if (activities.length >= 2) {
            return `${lastActivity}...`;
        }

        return null;
    }

    /**
     * Clean up stale sessions periodically
     */
    cleanup() {
        const sessions = this.loadSessions();
        const now = Date.now();
        const maxAge = 60 * 60 * 1000; // 1 hour
        let cleaned = 0;

        for (const [windowId, session] of Object.entries(sessions)) {
            if (now - session.lastActivity > maxAge) {
                delete sessions[windowId];
                cleaned++;
            }
        }

        if (cleaned > 0) {
            this.saveSessions(sessions);
            this.logActivity('cleanup', 'stale_cleanup', { cleaned });
        }
    }

    /**
     * Get all active sessions (for debugging)
     */
    getActiveSessions() {
        const sessions = this.loadSessions();
        const active = {};

        for (const [windowId, session] of Object.entries(sessions)) {
            if (session.active && this.isSessionActive(windowId)) {
                active[windowId] = session;
            }
        }

        return active;
    }

    /**
     * Force end all sessions (emergency cleanup)
     */
    endAllSessions(reason = 'force_cleanup') {
        const sessions = this.loadSessions();
        const now = Date.now();
        let ended = 0;

        for (const [windowId, session] of Object.entries(sessions)) {
            if (session.active) {
                session.active = false;
                session.endTime = now;
                session.endReason = reason;
                ended++;
            }
        }

        if (ended > 0) {
            this.saveSessions(sessions);
            this.logActivity('cleanup', 'force_end_all', { ended });
        }
    }

    /**
     * Log work session activity
     */
    logActivity(windowId, action, data = {}) {
        try {
            const logEntry = {
                timestamp: new Date().toISOString(),
                windowId,
                action,
                data,
                sessionId: getSessionId()
            };

            fs.appendFileSync('/tmp/work-session.log',
                JSON.stringify(logEntry) + '\n');
        } catch (e) {
            // Silent fail for logging
        }
    }

    /**
     * Log errors
     */
    logError(operation, error) {
        try {
            const logEntry = {
                timestamp: new Date().toISOString(),
                type: 'error',
                operation,
                error: error.message,
                sessionId: getSessionId()
            };

            fs.appendFileSync('/tmp/work-session.log',
                JSON.stringify(logEntry) + '\n');
        } catch (e) {
            // Silent fail for logging
        }
    }
}

// Global instance
let workSessionManager = null;

/**
 * Get the work session manager instance
 */
function getWorkSessionManager() {
    if (!workSessionManager) {
        workSessionManager = new WorkSessionManager();

        // Periodic cleanup (every 10th access)
        if (Math.random() < 0.1) {
            workSessionManager.cleanup();
        }
    }

    return workSessionManager;
}

/**
 * Convenience functions for common operations
 */

function startWorkSession(toolName, toolInput = {}, context = {}) {
    const windowId = getWindowId();
    const manager = getWorkSessionManager();

    // Check if we should resume a sleeping session instead of starting new
    if (manager.isSessionSleeping(windowId)) {
        return manager.resumeSession(windowId, toolName, toolInput, context);
    } else {
        return manager.startSession(windowId, toolName, toolInput, context);
    }
}

function isWorkSessionActive(windowId = null) {
    const targetWindowId = windowId || getWindowId();
    const manager = getWorkSessionManager();
    return manager.isSessionActive(targetWindowId);
}

function endWorkSession(reason = 'stop_hook', windowId = null) {
    const targetWindowId = windowId || getWindowId();
    const manager = getWorkSessionManager();
    return manager.endSession(targetWindowId, reason);
}

function getWorkSessionStatus(windowId = null) {
    const targetWindowId = windowId || getWindowId();
    const manager = getWorkSessionManager();
    return manager.getWorkSessionStatus(targetWindowId);
}

function getWorkSessionInfo(windowId = null) {
    const targetWindowId = windowId || getWindowId();
    const manager = getWorkSessionManager();
    return manager.getSessionInfo(targetWindowId);
}

function cleanupWorkSessions() {
    const manager = getWorkSessionManager();
    return manager.cleanup();
}

function getActiveWorkSessions() {
    const manager = getWorkSessionManager();
    return manager.getActiveSessions();
}

function endAllWorkSessions(reason = 'force_cleanup') {
    const manager = getWorkSessionManager();
    return manager.endAllSessions(reason);
}

function isWorkSessionSleeping(windowId = null) {
    const targetWindowId = windowId || getWindowId();
    const manager = getWorkSessionManager();
    return manager.isSessionSleeping(targetWindowId);
}

function resumeWorkSession(toolName, toolInput = {}, context = {}, windowId = null) {
    const targetWindowId = windowId || getWindowId();
    const manager = getWorkSessionManager();
    return manager.resumeSession(targetWindowId, toolName, toolInput, context);
}

module.exports = {
    WorkSessionManager,
    getWorkSessionManager,

    // Convenience functions
    startWorkSession,
    isWorkSessionActive,
    endWorkSession,
    getWorkSessionStatus,
    getWorkSessionInfo,
    cleanupWorkSessions,
    getActiveWorkSessions,
    endAllWorkSessions,
    isWorkSessionSleeping,
    resumeWorkSession
};