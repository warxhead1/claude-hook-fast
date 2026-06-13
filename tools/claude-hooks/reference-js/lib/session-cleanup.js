#!/usr/bin/env node

/**
 * Session Cleanup - Comprehensive cleanup of window/session state
 *
 * Handles cleanup of:
 * - Progress daemons and timing trackers
 * - Approval alerts and alerter daemons
 * - Temporary progress files
 * - Any other session-related state
 *
 * Used by pre-tool-use (clean previous tool state) and stop hooks (clean all state)
 */

const { execSync } = require('child_process');
const fs = require('fs');
const { alerter } = require('./approval-alerter-simple');

class SessionCleanup {
    constructor() {
        this.timingFile = '/tmp/claude-operation-timings.json';
        this.sessionDetectionFile = '/tmp/claude-session-detection.json';
    }

    /**
     * Detect if this is a new conversation session vs same-conversation tool change
     * Uses multiple heuristics to determine session boundaries
     */
    isNewSession(windowId) {
        try {
            const now = Date.now();
            const detectionData = this.loadSessionDetection();
            const windowKey = `window_${windowId}`;
            const currentProcess = {
                pid: process.pid,
                ppid: process.ppid,
                timestamp: now
            };

            // Check if we have previous session data for this window
            if (!detectionData[windowKey]) {
                // First time seeing this window - definitely new session
                detectionData[windowKey] = currentProcess;
                this.saveSessionDetection(detectionData);
                return true;
            }

            const lastSession = detectionData[windowKey];

            // Heuristic 1: Parent process ID changed (new Claude Code instance)
            if (lastSession.ppid !== currentProcess.ppid) {
                fs.appendFileSync('/tmp/tmux-hook.log',
                    `${new Date().toISOString()} - SessionDetection: New session detected (PPID changed: ${lastSession.ppid} → ${currentProcess.ppid})\n`);
                detectionData[windowKey] = currentProcess;
                this.saveSessionDetection(detectionData);
                return true;
            }

            // Heuristic 2: Long time gap (>30 minutes = likely new conversation)
            const timeSinceLastActivity = now - lastSession.timestamp;
            const maxSessionGap = 30 * 60 * 1000; // 30 minutes
            if (timeSinceLastActivity > maxSessionGap) {
                fs.appendFileSync('/tmp/tmux-hook.log',
                    `${new Date().toISOString()} - SessionDetection: New session detected (time gap: ${Math.round(timeSinceLastActivity/60000)}min)\n`);
                detectionData[windowKey] = currentProcess;
                this.saveSessionDetection(detectionData);
                return true;
            }

            // Heuristic 3: Session stats file is stale (created >30min ago but not updated recently)
            const sessionStatsFile = `/tmp/tmux-session-stats-${windowId}.json`;
            if (fs.existsSync(sessionStatsFile)) {
                const stats = fs.statSync(sessionStatsFile);
                const statAge = now - stats.mtime.getTime();
                const statsContent = JSON.parse(fs.readFileSync(sessionStatsFile, 'utf8'));
                const lastUpdate = statsContent.lastUpdate || stats.mtime.getTime();
                const updateAge = now - lastUpdate;

                // If stats file is old AND hasn't been updated recently
                if (statAge > maxSessionGap && updateAge > maxSessionGap) {
                    fs.appendFileSync('/tmp/tmux-hook.log',
                        `${new Date().toISOString()} - SessionDetection: New session detected (stale session stats: ${Math.round(statAge/60000)}min old)\n`);
                    detectionData[windowKey] = currentProcess;
                    this.saveSessionDetection(detectionData);
                    return true;
                }
            }

            // Update last activity timestamp for this session
            detectionData[windowKey].timestamp = now;
            this.saveSessionDetection(detectionData);
            return false;

        } catch (error) {
            fs.appendFileSync('/tmp/tmux-hook.log',
                `${new Date().toISOString()} - SessionDetection: Error detecting session: ${error.message}\n`);
            // On error, assume new session for safety
            return true;
        }
    }

    loadSessionDetection() {
        try {
            if (fs.existsSync(this.sessionDetectionFile)) {
                return JSON.parse(fs.readFileSync(this.sessionDetectionFile, 'utf8'));
            }
        } catch (e) {}
        return {};
    }

    saveSessionDetection(data) {
        try {
            fs.writeFileSync(this.sessionDetectionFile, JSON.stringify(data, null, 2));
        } catch (e) {}
    }

    /**
     * Get session data by session_id (preferred method)
     */
    getSessionData(sessionId) {
        try {
            const sessionFile = `/tmp/claude-session-${sessionId}.json`;
            if (fs.existsSync(sessionFile)) {
                return JSON.parse(fs.readFileSync(sessionFile, 'utf8'));
            }
        } catch (e) {}
        return null;
    }

    /**
     * Update session data by session_id
     */
    updateSessionData(sessionId, data) {
        try {
            const sessionFile = `/tmp/claude-session-${sessionId}.json`;
            fs.writeFileSync(sessionFile, JSON.stringify(data, null, 2));
            return true;
        } catch (e) {
            fs.appendFileSync('/tmp/tmux-hook.log',
                `${new Date().toISOString()} - SessionCleanup: Error updating session data: ${e.message}\n`);
            return false;
        }
    }

    /**
     * Clean up by session_id (preferred method for session-aware cleanup)
     */
    cleanupSession(sessionId, reason = 'session-cleanup') {
        const sessionData = this.getSessionData(sessionId);
        if (!sessionData) {
            fs.appendFileSync('/tmp/tmux-hook.log',
                `${new Date().toISOString()} - SessionCleanup: No session data found for ${sessionId}\n`);
            return null;
        }

        const windowId = sessionData.currentWindowId;
        fs.appendFileSync('/tmp/tmux-hook.log',
            `${new Date().toISOString()} - SessionCleanup: Cleaning session ${sessionId} in window ${windowId}\n`);

        // Clean the current window where this session is active
        return this.cleanupWindow(windowId, reason, false);
    }

    /**
     * Comprehensive cleanup for a specific window/session
     * @param {string} windowId - The window ID to clean up
     * @param {string} reason - Reason for cleanup (for logging)
     * @param {boolean} checkNewSession - Whether to check for new session and reset stats
     */
    cleanupWindow(windowId, reason = 'session-cleanup', checkNewSession = false) {
        const startTime = Date.now();

        fs.appendFileSync('/tmp/tmux-hook.log',
            `${new Date().toISOString()} - SessionCleanup: Starting cleanup for window ${windowId}, reason: ${reason}\n`);

        let cleaned = {
            progressDaemons: 0,
            orphanedDaemons: 0,
            staleOperations: 0,
            timingOperations: 0,
            progressFiles: 0,
            approvalAlerts: 0,
            alerterDaemons: 0,
            sessionReset: false
        };

        try {
            // Check if this is a new session first
            let isNewSession = false;
            if (checkNewSession) {
                isNewSession = this.isNewSession(windowId);
                if (isNewSession) {
                    fs.appendFileSync('/tmp/tmux-hook.log',
                        `${new Date().toISOString()} - SessionCleanup: NEW SESSION detected for window ${windowId}\n`);
                }
            }

            // 1. Kill progress daemons for this window
            cleaned.progressDaemons = this.killProgressDaemons(windowId);

            // 2. Kill orphaned progress daemons
            cleaned.orphanedDaemons = this.killOrphanedProgressDaemons();

            // 3. Clean stale timing operations
            cleaned.staleOperations = this.cleanStaleTimingOperations();

            // 4. Clean timing tracker operations for this window
            cleaned.timingOperations = this.cleanTimingOperations(windowId);

            // 5. Clean up progress files for this window
            cleaned.progressFiles = this.cleanProgressFiles(windowId);

            // 6. Clear approval alerts for this window
            cleaned.approvalAlerts = this.clearApprovalAlerts(windowId);

            // 7. Clean up alerter daemons if no alerts remain
            cleaned.alerterDaemons = this.cleanAlerterDaemons();

            // 8. Reset session stats if this is a new session
            if (isNewSession) {
                cleaned.sessionReset = this.resetSessionStats(windowId);
            }

            const duration = Date.now() - startTime;
            fs.appendFileSync('/tmp/tmux-hook.log',
                `${new Date().toISOString()} - SessionCleanup: Completed cleanup for window ${windowId} in ${duration}ms: ${JSON.stringify(cleaned)}\n`);

            return cleaned;
        } catch (error) {
            fs.appendFileSync('/tmp/tmux-hook.log',
                `${new Date().toISOString()} - SessionCleanup: Error during cleanup for window ${windowId}: ${error.message}\n`);
            return cleaned;
        }
    }

    /**
     * Kill all progress daemons for a specific window
     */
    killProgressDaemons(windowId) {
        let killed = 0;
        try {
            // Find progress daemons with window ID in their argument
            const processes = execSync('ps aux | grep progress-daemon', { encoding: 'utf8' });
            const lines = processes.split('\n');

            for (const line of lines) {
                if (line.includes(`${windowId}_`) && !line.includes('grep')) {
                    // Extract PID (second column)
                    const parts = line.trim().split(/\s+/);
                    const pid = parts[1];

                    try {
                        execSync(`kill ${pid}`, { stdio: 'ignore' });
                        killed++;
                        fs.appendFileSync('/tmp/tmux-hook.log',
                            `${new Date().toISOString()} - SessionCleanup: Killed progress daemon PID ${pid} for window ${windowId}\n`);
                    } catch (e) {
                        // Process might already be dead
                    }
                }
            }
        } catch (e) {
            // No processes found or grep failed - that's fine
        }
        return killed;
    }

    /**
     * Kill orphaned progress daemons - those running for operations that no longer exist
     */
    killOrphanedProgressDaemons() {
        let killed = 0;
        try {
            // Get current timing data
            const timingData = fs.existsSync(this.timingFile) ?
                JSON.parse(fs.readFileSync(this.timingFile, 'utf8')) : {};
            const activeOperations = new Set(Object.keys(timingData));

            // Find all progress daemons
            const processes = execSync('ps aux | grep progress-daemon', { encoding: 'utf8' });
            const lines = processes.split('\n');

            for (const line of lines) {
                if (line.includes('progress-daemon.js') && !line.includes('grep')) {
                    // Extract the operation key from the command line
                    const match = line.match(/progress-daemon\.js\s+(\S+)/);
                    if (match) {
                        const operationKey = match[1];

                        // Check if this operation still exists
                        if (!activeOperations.has(operationKey)) {
                            // Orphaned daemon - kill it
                            const parts = line.trim().split(/\s+/);
                            const pid = parts[1];

                            try {
                                execSync(`kill ${pid}`, { stdio: 'ignore' });
                                killed++;
                                fs.appendFileSync('/tmp/tmux-hook.log',
                                    `${new Date().toISOString()} - SessionCleanup: Killed orphaned progress daemon PID ${pid} for operation ${operationKey}\n`);
                            } catch (e) {
                                // Process might already be dead
                            }
                        }
                    }
                }
            }
        } catch (e) {
            fs.appendFileSync('/tmp/tmux-hook.log',
                `${new Date().toISOString()} - SessionCleanup: Error killing orphaned daemons: ${e.message}\n`);
        }
        return killed;
    }

    /**
     * Clean stale timing operations (older than 30 minutes with no recent activity)
     */
    cleanStaleTimingOperations() {
        let cleaned = 0;
        try {
            if (!fs.existsSync(this.timingFile)) return 0;

            const data = JSON.parse(fs.readFileSync(this.timingFile, 'utf8'));
            const now = Date.now();
            const staleThreshold = 30 * 60 * 1000; // 30 minutes

            for (const [key, operation] of Object.entries(data)) {
                const age = now - operation.startTime;
                if (age > staleThreshold) {
                    // Check if the process is actually still running
                    let processStillRunning = false;

                    if (operation.toolName === 'Bash' && operation.toolInput?.command) {
                        const cmd = operation.toolInput.command;
                        if (cmd.includes('cargo')) {
                            try {
                                execSync('pgrep -f cargo', { encoding: 'utf8' });
                                processStillRunning = true;
                            } catch (e) {
                                // No cargo processes running
                            }
                        }
                    }

                    if (!processStillRunning) {
                        delete data[key];
                        cleaned++;
                        fs.appendFileSync('/tmp/tmux-hook.log',
                            `${new Date().toISOString()} - SessionCleanup: Cleaned stale operation ${key} (age: ${Math.round(age/60000)}min)\n`);
                    }
                }
            }

            if (cleaned > 0) {
                fs.writeFileSync(this.timingFile, JSON.stringify(data, null, 2));
            }
        } catch (e) {
            fs.appendFileSync('/tmp/tmux-hook.log',
                `${new Date().toISOString()} - SessionCleanup: Error cleaning stale operations: ${e.message}\n`);
        }
        return cleaned;
    }

    /**
     * Remove timing operations for a specific window
     */
    cleanTimingOperations(windowId) {
        let removed = 0;
        try {
            if (!fs.existsSync(this.timingFile)) return 0;

            const data = JSON.parse(fs.readFileSync(this.timingFile, 'utf8'));
            const before = Object.keys(data).length;

            // Remove all operations for this window
            for (const [key, operation] of Object.entries(data)) {
                if (operation.windowId === windowId || key.startsWith(`${windowId}_`)) {
                    delete data[key];
                    removed++;
                }
            }

            if (removed > 0) {
                fs.writeFileSync(this.timingFile, JSON.stringify(data, null, 2));
                fs.appendFileSync('/tmp/tmux-hook.log',
                    `${new Date().toISOString()} - SessionCleanup: Removed ${removed} timing operations for window ${windowId}\n`);
            }
        } catch (e) {
            fs.appendFileSync('/tmp/tmux-hook.log',
                `${new Date().toISOString()} - SessionCleanup: Error cleaning timing operations: ${e.message}\n`);
        }
        return removed;
    }

    /**
     * Clean up progress files for a specific window
     */
    cleanProgressFiles(windowId) {
        let removed = 0;
        try {
            // Find progress files that match the window ID pattern
            const files = execSync('ls /tmp/progress-* 2>/dev/null || true', { encoding: 'utf8' });
            const fileList = files.split('\n').filter(f => f.trim());

            for (const file of fileList) {
                if (file.includes(`${windowId}_`)) {
                    try {
                        fs.unlinkSync(file);
                        removed++;
                        fs.appendFileSync('/tmp/tmux-hook.log',
                            `${new Date().toISOString()} - SessionCleanup: Removed progress file ${file}\n`);
                    } catch (e) {
                        // File might already be deleted
                    }
                }
            }
        } catch (e) {
            // No files found or ls failed - that's fine
        }
        return removed;
    }

    /**
     * Clear approval alerts for all windows (session-wide cleanup)
     */
    clearApprovalAlerts(windowId) {
        let cleared = 0;
        try {
            const alerts = alerter.loadAlerts();
            const clearedWindows = [];

            // Clear ALL approval alerts, not just for this window
            // This fixes the issue where notification hook creates alert in window X
            // but post-tool-use cleanup runs in window Y
            for (const alertWindowId of Object.keys(alerts)) {
                delete alerts[alertWindowId];
                clearedWindows.push(alertWindowId);
                cleared++;
            }

            if (cleared > 0) {
                alerter.saveAlerts(alerts);
                fs.appendFileSync('/tmp/tmux-hook.log',
                    `${new Date().toISOString()} - SessionCleanup: Cleared approval alerts for windows: ${clearedWindows.join(', ')} (triggered from window ${windowId})\n`);
            }
        } catch (e) {
            fs.appendFileSync('/tmp/tmux-hook.log',
                `${new Date().toISOString()} - SessionCleanup: Error clearing approval alerts: ${e.message}\n`);
        }
        return cleared;
    }

    /**
     * Reset session statistics for a new conversation session
     */
    resetSessionStats(windowId) {
        try {
            const sessionStatsFile = `/tmp/tmux-session-stats-${windowId}.json`;

            // Create fresh session stats with current timestamp
            const newStats = {
                startTime: Date.now(),
                operations: 0,
                lastUpdate: Date.now(),
                windowId: windowId
            };

            fs.writeFileSync(sessionStatsFile, JSON.stringify(newStats, null, 2));

            fs.appendFileSync('/tmp/tmux-hook.log',
                `${new Date().toISOString()} - SessionCleanup: Reset session stats for window ${windowId}\n`);

            return true;
        } catch (e) {
            fs.appendFileSync('/tmp/tmux-hook.log',
                `${new Date().toISOString()} - SessionCleanup: Error resetting session stats: ${e.message}\n`);
            return false;
        }
    }

    /**
     * Clean up alerter daemons if no alerts remain
     */
    cleanAlerterDaemons() {
        let killed = 0;
        try {
            const alerts = alerter.loadAlerts();
            const activeAlerts = Object.keys(alerts).length;

            if (activeAlerts === 0) {
                // No alerts remain, kill all alerter daemons
                try {
                    const result = execSync('pkill -f "simple-alerter-daemon|alerter-daemon"', { encoding: 'utf8' });
                    // Count lines to estimate killed processes
                    killed = 1; // At least one was killed if pkill succeeded
                    fs.appendFileSync('/tmp/tmux-hook.log',
                        `${new Date().toISOString()} - SessionCleanup: Killed alerter daemons (no active alerts)\n`);
                } catch (e) {
                    // No daemons to kill - that's fine
                }
            }
        } catch (e) {
            // Error checking alerts - that's fine
        }
        return killed;
    }

    /**
     * Emergency cleanup - clean ALL state for ALL windows
     */
    emergencyCleanup(reason = 'emergency') {
        fs.appendFileSync('/tmp/tmux-hook.log',
            `${new Date().toISOString()} - SessionCleanup: EMERGENCY cleanup started, reason: ${reason}\n`);

        try {
            // Kill all progress daemons
            execSync('pkill -f progress-daemon', { stdio: 'ignore' });

            // Kill all alerter daemons
            execSync('pkill -f "simple-alerter-daemon|alerter-daemon"', { stdio: 'ignore' });

            // Clear timing file
            fs.writeFileSync(this.timingFile, '{}');

            // Clear approval alerts
            alerter.saveAlerts({});

            // Remove all progress files
            execSync('rm -f /tmp/progress-*.json', { stdio: 'ignore' });

            fs.appendFileSync('/tmp/tmux-hook.log',
                `${new Date().toISOString()} - SessionCleanup: EMERGENCY cleanup completed\n`);
        } catch (e) {
            fs.appendFileSync('/tmp/tmux-hook.log',
                `${new Date().toISOString()} - SessionCleanup: EMERGENCY cleanup error: ${e.message}\n`);
        }
    }

    /**
     * Get cleanup statistics for debugging
     */
    getCleanupStats() {
        try {
            const stats = {
                timingOperations: 0,
                approvalAlerts: 0,
                progressFiles: 0,
                progressDaemons: 0,
                alerterDaemons: 0
            };

            // Count timing operations
            if (fs.existsSync(this.timingFile)) {
                const data = JSON.parse(fs.readFileSync(this.timingFile, 'utf8'));
                stats.timingOperations = Object.keys(data).length;
            }

            // Count approval alerts
            const alerts = alerter.loadAlerts();
            stats.approvalAlerts = Object.keys(alerts).length;

            // Count progress files
            try {
                const files = execSync('ls /tmp/progress-*.json 2>/dev/null | wc -l', { encoding: 'utf8' });
                stats.progressFiles = parseInt(files.trim()) || 0;
            } catch (e) {}

            // Count daemons
            try {
                const processes = execSync('ps aux | grep -E "(progress-daemon|alerter-daemon)" | grep -v grep | wc -l', { encoding: 'utf8' });
                stats.progressDaemons = parseInt(processes.trim()) || 0;
            } catch (e) {}

            return stats;
        } catch (e) {
            return null;
        }
    }
}

// Export singleton
const sessionCleanup = new SessionCleanup();

module.exports = {
    SessionCleanup,
    sessionCleanup
};