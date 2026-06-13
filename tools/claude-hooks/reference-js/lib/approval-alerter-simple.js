#!/usr/bin/env node

/**
 * SIMPLIFIED Approval Alerter - BULLETPROOF clearing with minimal complexity
 *
 * Key principles:
 * 1. Clearing ALWAYS works - no complex logic
 * 2. No race conditions - immediate file-based communication
 * 3. Fail-safe behavior - errors default to cleared state
 * 4. Minimal daemon complexity
 */

const { execSync, spawn } = require('child_process');
const fs = require('fs');
const path = require('path');
const { updateApprovalPending, updateTmuxWindow, clearApprovalWithWorkSessionAwareness } = require('./tmux-status-coordinator');

class SimpleApprovalAlerter {
    constructor() {
        this.alertFile = '/tmp/claude-approval-alerts.json';
    }

    loadAlerts() {
        try {
            if (fs.existsSync(this.alertFile)) {
                return JSON.parse(fs.readFileSync(this.alertFile, 'utf8'));
            }
        } catch (e) {
            // If file is corrupted, return empty - fail-safe
        }
        return {};
    }

    saveAlerts(alerts) {
        try {
            fs.writeFileSync(this.alertFile, JSON.stringify(alerts));
        } catch (e) {
            // If we can't save, that's actually good for clearing - fail-safe
        }
    }

    startApprovalAlert(windowId, toolName, baseStatus) {
        // Log for debugging
        fs.appendFileSync('/tmp/tmux-hook.log',
            `${new Date().toISOString()} - SIMPLE: Starting approval alert for window ${windowId}\n`);

        // IMMEDIATE: Clear any existing alert first
        this.stopAlert(windowId);

        // Create new alert
        const alert = {
            windowId,
            toolName,
            baseStatus,
            startTime: Date.now(),
            active: true
        };

        // Save alert
        const alerts = this.loadAlerts();
        alerts[windowId] = alert;
        this.saveAlerts(alerts);

        // Use coordinator for immediate visual feedback
        updateApprovalPending(toolName);

        // IMMEDIATE: Show the alert — coordinator formats as "⚠ APPROVE"
        updateTmuxWindow("APPROVAL", windowId, 'approval-start-immediate');

        // Start simple daemon if needed for ongoing animation
        this.ensureDaemon();
    }

    ensureDaemon() {
        try {
            // Check if daemon is already running
            execSync('pgrep -f simple-alerter-daemon', { stdio: 'ignore' });
            // If we get here, daemon is running
            return;
        } catch (e) {
            // Daemon not running, start it
        }

        try {
            const daemonPath = path.join(__dirname, 'simple-alerter-daemon.js');

            // Spawn simple daemon
            const daemon = spawn('node', [daemonPath], {
                detached: true,
                stdio: 'ignore'
            });

            daemon.unref();

            fs.appendFileSync('/tmp/tmux-hook.log',
                `${new Date().toISOString()} - SIMPLE: Started daemon PID ${daemon.pid}\n`);
        } catch (e) {
            // If daemon fails to start, that's ok - clearing will still work
        }
    }

    stopAlert(windowId, setReady = false) {
        // BULLETPROOF CLEARING - this MUST work

        fs.appendFileSync('/tmp/tmux-hook.log',
            `${new Date().toISOString()} - SIMPLE: Stopping alert for window ${windowId}, setReady=${setReady}\n`);

        // 1. IMMEDIATE FILE REMOVAL - daemon will see this instantly
        const alerts = this.loadAlerts();
        if (alerts[windowId]) {
            delete alerts[windowId];
            this.saveAlerts(alerts);
        }

        // 2. IMMEDIATE VISUAL CLEARING - with work session awareness
        try {
            if (setReady) {
                // Explicitly requested to set Ready (e.g., from Stop hook)
                updateTmuxWindow("✓ ready", windowId, 'approval-alerter-clear');
                fs.appendFileSync('/tmp/tmux-hook.log',
                    `${new Date().toISOString()} - SIMPLE: Window ${windowId} cleared to Ready (explicit)\n`);
            } else {
                // Normal approval clearing - check work session status
                clearApprovalWithWorkSessionAwareness(windowId);
                fs.appendFileSync('/tmp/tmux-hook.log',
                    `${new Date().toISOString()} - SIMPLE: Window ${windowId} alert cleared with work session awareness\n`);
            }
        } catch (e) {
            fs.appendFileSync('/tmp/tmux-hook.log',
                `${new Date().toISOString()} - SIMPLE: Clear error for window ${windowId}: ${e.message}\n`);
        }

        // 3. CLEANUP: If no alerts remain, kill daemon
        const remainingAlerts = this.loadAlerts();
        if (Object.keys(remainingAlerts).length === 0) {
            try {
                execSync('pkill -f simple-alerter-daemon', { stdio: 'ignore' });
                fs.appendFileSync('/tmp/tmux-hook.log',
                    `${new Date().toISOString()} - SIMPLE: All alerts cleared, killed daemon\n`);
            } catch (e) {
                // Daemon kill can fail, that's ok
            }
        }
    }

    // EMERGENCY: Force clear everything
    clearAll() {
        try {
            // Nuke the alert file
            if (fs.existsSync(this.alertFile)) {
                fs.unlinkSync(this.alertFile);
            }

            // Kill all daemons
            execSync('pkill -f simple-alerter-daemon', { stdio: 'ignore' });

            // Clear all window names that look like alerts
            const windows = execSync('tmux list-windows -F "#{window_index}"', { encoding: 'utf8' })
                .trim().split('\n').filter(w => w);

            for (const windowId of windows) {
                try {
                    updateTmuxWindow("✓ ready", windowId, 'approval-alerter-emergency');
                } catch (e) {}
            }

            fs.appendFileSync('/tmp/tmux-hook.log',
                `${new Date().toISOString()} - SIMPLE: EMERGENCY clear all completed\n`);
        } catch (e) {
            // Even emergency clear can fail, log it
            fs.appendFileSync('/tmp/tmux-hook.log',
                `${new Date().toISOString()} - SIMPLE: Emergency clear error: ${e.message}\n`);
        }
    }
}

// Export singleton
const simpleAlerter = new SimpleApprovalAlerter();

module.exports = {
    SimpleApprovalAlerter,
    alerter: simpleAlerter
};