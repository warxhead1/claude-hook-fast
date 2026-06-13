#!/usr/bin/env node

/**
 * Status History Logger
 * Tracks operations and their durations for session statistics
 */

const fs = require('fs');
const path = require('path');

const HISTORY_FILE = '/tmp/claude-status-history.jsonl';
const MAX_HISTORY_SIZE = 1000; // Keep last 1000 entries

// Use window-specific session stats file
function getSessionStatsFile(windowId) {
    return `/tmp/tmux-session-stats-${windowId || 'default'}.json`;
}

class StatusHistory {
    constructor(windowId) {
        this.windowId = windowId || 'default';
        this.sessionStatsFile = getSessionStatsFile(this.windowId);
        // Use process start time or current time
        this.sessionStart = Date.now();
        this.operations = 0;
        // Try to get session ID from environment or use PID
        this.sessionId = process.env.CLAUDE_SESSION_ID || process.ppid;
        this.loadSessionStats();
    }

    loadSessionStats() {
        try {
            if (fs.existsSync(this.sessionStatsFile)) {
                const stats = JSON.parse(fs.readFileSync(this.sessionStatsFile, 'utf8'));
                this.sessionStart = stats.startTime || Date.now();
                this.operations = stats.operations || 0;
            } else {
                this.saveSessionStats();
            }
        } catch (e) {
            this.saveSessionStats();
        }
    }

    saveSessionStats() {
        try {
            fs.writeFileSync(this.sessionStatsFile, JSON.stringify({
                startTime: this.sessionStart,
                operations: this.operations,
                lastUpdate: Date.now(),
                windowId: this.windowId
            }));
        } catch (e) {}
    }

    recordOperation(type, tool, status, details = {}) {
        try {
            const entry = {
                timestamp: Date.now(),
                type,
                tool,
                status,
                windowId: details.windowId,
                pid: process.pid,
                ppid: process.ppid,
                ...details
            };

            // Append to history file
            fs.appendFileSync(HISTORY_FILE, JSON.stringify(entry) + '\n');

            // Update operation count
            if (type === 'post' && status === 'success') {
                this.operations++;
                this.saveSessionStats();
            }

            // Trim history file if too large
            this.trimHistory();

        } catch (e) {}
    }

    trimHistory() {
        try {
            const lines = fs.readFileSync(HISTORY_FILE, 'utf8').split('\n').filter(l => l);
            if (lines.length > MAX_HISTORY_SIZE) {
                const trimmed = lines.slice(-MAX_HISTORY_SIZE);
                fs.writeFileSync(HISTORY_FILE, trimmed.join('\n') + '\n');
            }
        } catch (e) {}
    }

    getRecentOperations(count = 10) {
        try {
            const lines = fs.readFileSync(HISTORY_FILE, 'utf8').split('\n').filter(l => l);
            return lines.slice(-count).map(l => JSON.parse(l));
        } catch (e) {
            return [];
        }
    }

    getOperationDuration(startTime, endTime) {
        const duration = endTime - startTime;
        if (duration < 1000) return `${duration}ms`;
        if (duration < 60000) return `${Math.floor(duration / 1000)}s`;
        return `${Math.floor(duration / 60000)}m`;
    }

    getSessionDuration() {
        const elapsed = Date.now() - this.sessionStart;
        const minutes = Math.floor(elapsed / 60000);
        const seconds = Math.floor((elapsed % 60000) / 1000);
        if (minutes > 0) {
            return `${minutes}m ${seconds}s`;
        }
        return `${seconds}s`;
    }

    analyzeLongRunning(thresholdMs = 5000) {
        const recent = this.getRecentOperations(100);
        const longRunning = [];

        for (let i = 0; i < recent.length - 1; i++) {
            if (recent[i].type === 'pre' && recent[i + 1].type === 'post' &&
                recent[i].tool === recent[i + 1].tool) {
                const duration = recent[i + 1].timestamp - recent[i].timestamp;
                if (duration > thresholdMs) {
                    longRunning.push({
                        tool: recent[i].tool,
                        duration: this.getOperationDuration(recent[i].timestamp, recent[i + 1].timestamp),
                        durationMs: duration,
                        details: recent[i].details
                    });
                }
            }
        }

        return longRunning.sort((a, b) => b.durationMs - a.durationMs);
    }
}

module.exports = {
    StatusHistory
};