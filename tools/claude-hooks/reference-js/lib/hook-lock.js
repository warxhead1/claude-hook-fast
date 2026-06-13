#!/usr/bin/env node

/**
 * Hook Lock Manager - Coordinates parallel hook execution
 *
 * Each hook acquires a lock at start and releases at end.
 * Feedback collector waits for all locks to be released.
 */

const fs = require('fs');
const path = require('path');

const MAX_WAIT = 5000; // 5 seconds max wait

class HookLock {
    constructor(hookName, sessionId = null) {
        this.hookName = hookName;
        this.sessionId = sessionId;

        // Use session-isolated lock directory (following existing pattern)
        const lockDir = sessionId ? `/tmp/claude-hook-locks-${sessionId}` : '/tmp/claude-hook-locks';
        this.lockDir = lockDir;
        this.lockFile = path.join(lockDir, `${hookName}-${process.pid}.lock`);

        // Ensure lock directory exists
        if (!fs.existsSync(this.lockDir)) {
            fs.mkdirSync(this.lockDir, { recursive: true });
        }
    }

    /**
     * Acquire a lock for this hook
     */
    acquire() {
        try {
            fs.writeFileSync(this.lockFile, JSON.stringify({
                hook: this.hookName,
                pid: process.pid,
                timestamp: Date.now()
            }));
            return true;
        } catch (e) {
            return false;
        }
    }

    /**
     * Release this hook's lock
     */
    release() {
        try {
            if (fs.existsSync(this.lockFile)) {
                fs.unlinkSync(this.lockFile);
            }
            return true;
        } catch (e) {
            return false;
        }
    }

    /**
     * Wait for all locks to be released (for feedback collector)
     */
    static async waitForAll(timeout = MAX_WAIT, sessionId = null) {
        const startTime = Date.now();
        const lockDir = sessionId ? `/tmp/claude-hook-locks-${sessionId}` : '/tmp/claude-hook-locks';

        while (Date.now() - startTime < timeout) {
            try {
                // Clean up stale locks (older than 10 seconds)
                HookLock.cleanStale(sessionId);

                // Check if any locks remain (session-isolated)
                if (!fs.existsSync(lockDir)) {
                    return true; // No lock directory = no locks
                }

                const locks = fs.readdirSync(lockDir).filter(f => f.endsWith('.lock'));

                if (locks.length === 0) {
                    return true; // All locks released
                }

                // Wait a bit before checking again
                await new Promise(resolve => setTimeout(resolve, 50));
            } catch (e) {
                return true; // Assume no locks if we can't read
            }
        }

        return false; // Timeout
    }

    /**
     * Clean up stale locks from crashed processes
     */
    static cleanStale(sessionId = null) {
        try {
            const lockDir = sessionId ? `/tmp/claude-hook-locks-${sessionId}` : '/tmp/claude-hook-locks';

            if (!fs.existsSync(lockDir)) {
                return; // No directory = nothing to clean
            }

            const now = Date.now();
            const files = fs.readdirSync(lockDir);

            for (const file of files) {
                if (!file.endsWith('.lock')) continue;

                try {
                    const lockPath = path.join(lockDir, file);
                    const data = JSON.parse(fs.readFileSync(lockPath, 'utf8'));

                    // Remove locks older than 10 seconds
                    if (now - data.timestamp > 10000) {
                        fs.unlinkSync(lockPath);
                    }
                } catch (e) {
                    // Remove unreadable locks
                    try {
                        fs.unlinkSync(path.join(lockDir, file));
                    } catch (e2) {}
                }
            }
        } catch (e) {}
    }

    /**
     * Get list of active locks
     */
    static getActive(sessionId = null) {
        try {
            const lockDir = sessionId ? `/tmp/claude-hook-locks-${sessionId}` : '/tmp/claude-hook-locks';

            if (!fs.existsSync(lockDir)) {
                return []; // No directory = no locks
            }

            const locks = [];
            const files = fs.readdirSync(lockDir).filter(f => f.endsWith('.lock'));

            for (const file of files) {
                try {
                    const data = JSON.parse(fs.readFileSync(path.join(lockDir, file), 'utf8'));
                    locks.push(data);
                } catch (e) {}
            }

            return locks;
        } catch (e) {
            return [];
        }
    }
}

module.exports = { HookLock };