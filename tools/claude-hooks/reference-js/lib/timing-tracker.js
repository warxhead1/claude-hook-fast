#!/usr/bin/env node

/**
 * Timing Tracker - Track operation durations and show elapsed time
 * For operations that take longer than expected
 */

const fs = require('fs');

const TIMING_DATA_FILE = '/tmp/claude-operation-timings.json';
const LONG_OPERATION_THRESHOLD = 3000; // 3 seconds

class TimingTracker {
    constructor() {
        this.activeOperations = this.loadTimings();
    }

    loadTimings() {
        try {
            if (fs.existsSync(TIMING_DATA_FILE)) {
                return JSON.parse(fs.readFileSync(TIMING_DATA_FILE, 'utf8'));
            }
        } catch (e) {}
        return {};
    }

    saveTimings() {
        try {
            fs.writeFileSync(TIMING_DATA_FILE, JSON.stringify(this.activeOperations));
        } catch (e) {}
    }

    startOperation(toolName, toolInput, windowId) {
        const operationKey = `${windowId}_${toolName}`;

        this.activeOperations[operationKey] = {
            toolName,
            toolInput,
            windowId,
            startTime: Date.now(),
            filename: toolInput.file_path ? toolInput.file_path.split('/').pop() : null,
            expectedLong: this.isExpectedLongOperation(toolName, toolInput)
        };

        this.saveTimings();

        // 📊 Start live progress tracking for expected long operations
        if (this.activeOperations[operationKey].expectedLong) {
            this.startProgressTracking(operationKey);
        }
    }

    isExpectedLongOperation(toolName, toolInput) {
        // Define operations that are typically long-running
        const longOperationPatterns = [
            // Cargo operations
            { tool: 'Bash', pattern: /cargo\s+(build|test|check|run)/ },
            { tool: 'mcp__tengine-refactor__build', pattern: /./ },

            // Semantic search operations
            { tool: 'mcp__tengine-refactor__search', pattern: /./ },
            { tool: 'mcp__tengine-refactor__call_chain', pattern: /./ },

            // Debug operations
            { tool: 'mcp__tengine-refactor__debug_analyzer', pattern: /./ },

            // Compilation operations
            { tool: 'Bash', pattern: /gcc|g\+\+|clang|rustc|javac/ },
            { tool: 'Bash', pattern: /npm\s+(install|build|test)/ },

            // Network operations
            { tool: 'WebFetch', pattern: /./ },
            { tool: 'WebSearch', pattern: /./ },
        ];

        for (const { tool, pattern } of longOperationPatterns) {
            if (toolName === tool) {
                const searchText = JSON.stringify(toolInput).toLowerCase();
                if (pattern.test(searchText)) {
                    return true;
                }
            }
        }

        return false;
    }

    startProgressTracking(operationKey) {
        // Spawn a detached daemon to update progress
        try {
            const { spawn } = require('child_process');
            const path = require('path');
            const daemonPath = path.join(__dirname, 'progress-daemon.js');

            // Pass operation key to daemon
            const daemon = spawn('node', [daemonPath, operationKey], {
                detached: true,
                stdio: 'ignore',
                env: { ...process.env }
            });

            daemon.unref();
        } catch (e) {
            // Silent fail if daemon can't be spawned
        }
    }

    completeOperation(toolName, windowId) {
        const operationKey = `${windowId}_${toolName}`;
        const operation = this.activeOperations[operationKey];

        if (operation) {
            const duration = Date.now() - operation.startTime;

            // 📊 Stop progress tracking if it was a long operation
            if (operation.expectedLong) {
                this.stopProgressTracking(operationKey);
            }

            delete this.activeOperations[operationKey];
            this.saveTimings();
            return duration;
        }

        return null;
    }

    stopProgressTracking(operationKey) {
        // Mark operation as completed so daemon will stop
        try {
            const progressFile = `/tmp/progress-${operationKey.replace(/[^a-zA-Z0-9]/g, '_')}.json`;
            fs.writeFileSync(progressFile, JSON.stringify({ completed: true, timestamp: Date.now() }));
        } catch (e) {}
    }

    getElapsedTime(toolName, windowId) {
        const operationKey = `${windowId}_${toolName}`;
        const operation = this.activeOperations[operationKey];

        if (operation) {
            return Date.now() - operation.startTime;
        }

        return null;
    }

    isLongRunning(toolName, windowId) {
        const elapsed = this.getElapsedTime(toolName, windowId);
        return elapsed && elapsed > LONG_OPERATION_THRESHOLD;
    }

    formatElapsedTime(milliseconds) {
        if (milliseconds < 1000) {
            return `${Math.round(milliseconds)}ms`;
        } else if (milliseconds < 60000) {
            return `${Math.round(milliseconds / 1000)}s`;
        } else {
            const minutes = Math.floor(milliseconds / 60000);
            const seconds = Math.round((milliseconds % 60000) / 1000);
            return `${minutes}m ${seconds}s`;
        }
    }

    // Test comment for approval demo - testing fixed scrollback preservation

    getTimingEnhancement(toolName, windowId, baseStatus) {
        const elapsed = this.getElapsedTime(toolName, windowId);

        if (!elapsed || elapsed < LONG_OPERATION_THRESHOLD) {
            return baseStatus;
        }

        const formattedTime = this.formatElapsedTime(elapsed);

        // Different timing indicators based on duration
        let timeIcon = '⏱️';
        if (elapsed > 30000) {
            timeIcon = '⌛'; // Very long operation
        } else if (elapsed > 10000) {
            timeIcon = '⏰'; // Long operation
        }

        return `${baseStatus} ${timeIcon} ${formattedTime}`;
    }

    // Clean up old operations (prevent memory leaks)
    cleanup() {
        const now = Date.now();
        const maxAge = 10 * 60 * 1000; // 10 minutes

        for (const [key, operation] of Object.entries(this.activeOperations)) {
            if (now - operation.startTime > maxAge) {
                delete this.activeOperations[key];
            }
        }

        this.saveTimings();
    }

    // Get statistics about operation timing patterns
    getTimingStats() {
        const operations = Object.values(this.activeOperations);
        if (operations.length === 0) return null;

        const now = Date.now();
        const longOps = operations.filter(op => now - op.startTime > LONG_OPERATION_THRESHOLD);

        return {
            activeOperations: operations.length,
            longRunningOperations: longOps.length,
            longestOperation: Math.max(...operations.map(op => now - op.startTime)),
            operationsByTool: operations.reduce((acc, op) => {
                acc[op.toolName] = (acc[op.toolName] || 0) + 1;
                return acc;
            }, {})
        };
    }
}

// Singleton instance
const timingTracker = new TimingTracker();

// Note: No setInterval here to avoid blocking hook processes
// Cleanup will be called manually when needed

module.exports = {
    TimingTracker,
    timingTracker,
    LONG_OPERATION_THRESHOLD
};