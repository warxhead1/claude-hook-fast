#!/usr/bin/env node

/**
 * Process Registry - Unified tracking of active Claude/Codex CLI processes
 *
 * Provides:
 * - Registration of active CLI processes with conversation linking
 * - Process type detection (claude, codex)
 * - Heartbeat mechanism to detect stale processes
 * - Unified view of active vs historical sessions
 * - Integration with conversation tracking
 */

const fs = require('fs');
const { execSync } = require('child_process');
const path = require('path');

class ProcessRegistry {
    constructor() {
        this.registryFile = '/tmp/claude-process-registry.json';
        this.heartbeatInterval = 30000; // 30 seconds
        this.staleTimeout = 120000; // 2 minutes
    }

    /**
     * Load the process registry
     */
    loadRegistry() {
        try {
            if (fs.existsSync(this.registryFile)) {
                return JSON.parse(fs.readFileSync(this.registryFile, 'utf8'));
            }
        } catch (e) {
            this.logError('loadRegistry', e);
        }
        return { processes: {}, metadata: { last_cleanup: Date.now() } };
    }

    /**
     * Save the process registry
     */
    saveRegistry(registry) {
        try {
            fs.writeFileSync(this.registryFile, JSON.stringify(registry, null, 2));
            return true;
        } catch (e) {
            this.logError('saveRegistry', e);
            return false;
        }
    }

    /**
     * Register a CLI process
     */
    registerProcess(pid, processInfo) {
        const registry = this.loadRegistry();
        const pidStr = pid.toString();

        const processEntry = {
            pid: parseInt(pid),
            type: processInfo.type || this.detectProcessType(pid),
            conversation_id: processInfo.conversation_id || null,
            window: processInfo.window || null,
            tty: processInfo.tty || this.getProcessTTY(pid),
            start_time: processInfo.start_time || Date.now(),
            last_heartbeat: Date.now(),
            command_line: processInfo.command_line || this.getProcessCommand(pid),
            parent_pid: processInfo.parent_pid || this.getParentPid(pid),
            session_id: processInfo.session_id || null,
            registered_time: Date.now()
        };

        registry.processes[pidStr] = processEntry;

        // Update metadata
        registry.metadata.last_update = Date.now();
        registry.metadata.process_count = Object.keys(registry.processes).length;

        this.saveRegistry(registry);

        this.logRegistration('register', processEntry);
        return processEntry;
    }

    /**
     * Update heartbeat for a process
     */
    heartbeat(pid, additionalInfo = {}) {
        const registry = this.loadRegistry();
        const pidStr = pid.toString();

        if (registry.processes[pidStr]) {
            registry.processes[pidStr].last_heartbeat = Date.now();

            // Update any additional info provided
            if (additionalInfo.conversation_id) {
                registry.processes[pidStr].conversation_id = additionalInfo.conversation_id;
            }
            if (additionalInfo.window) {
                registry.processes[pidStr].window = additionalInfo.window;
            }
            if (additionalInfo.operation_count) {
                registry.processes[pidStr].operation_count = additionalInfo.operation_count;
            }

            this.saveRegistry(registry);
            return true;
        }

        return false;
    }

    /**
     * Get process information
     */
    getProcess(pid) {
        const registry = this.loadRegistry();
        return registry.processes[pid.toString()] || null;
    }

    /**
     * Get all active processes
     */
    getActiveProcesses() {
        const registry = this.loadRegistry();
        const now = Date.now();
        const activeProcesses = {};

        for (const [pidStr, process] of Object.entries(registry.processes)) {
            // Check if process is still alive and not stale
            if (this.isProcessAlive(process.pid) &&
                (now - process.last_heartbeat) < this.staleTimeout) {
                activeProcesses[pidStr] = process;
            }
        }

        return activeProcesses;
    }

    /**
     * Get processes by type
     */
    getProcessesByType(type) {
        const activeProcesses = this.getActiveProcesses();
        const filtered = {};

        for (const [pidStr, process] of Object.entries(activeProcesses)) {
            if (process.type === type) {
                filtered[pidStr] = process;
            }
        }

        return filtered;
    }

    /**
     * Get process by conversation ID
     */
    getProcessByConversation(conversationId) {
        const activeProcesses = this.getActiveProcesses();

        for (const process of Object.values(activeProcesses)) {
            if (process.conversation_id === conversationId) {
                return process;
            }
        }

        return null;
    }

    /**
     * Clean up stale and dead processes
     */
    cleanup() {
        const registry = this.loadRegistry();
        const now = Date.now();
        let cleaned = 0;

        for (const [pidStr, process] of Object.entries(registry.processes)) {
            const isStale = (now - process.last_heartbeat) > this.staleTimeout;
            const isDead = !this.isProcessAlive(process.pid);

            if (isStale || isDead) {
                this.logRegistration('cleanup', process, { reason: isStale ? 'stale' : 'dead' });
                delete registry.processes[pidStr];
                cleaned++;
            }
        }

        if (cleaned > 0) {
            registry.metadata.last_cleanup = now;
            registry.metadata.process_count = Object.keys(registry.processes).length;
            this.saveRegistry(registry);

            this.logActivity('cleanup', { cleaned_count: cleaned });
        }

        return cleaned;
    }

    /**
     * Detect process type (claude, codex, unknown)
     */
    detectProcessType(pid) {
        try {
            const cmdline = this.getProcessCommand(pid);
            if (!cmdline) return 'unknown';

            const cmd = cmdline.toLowerCase();
            if (cmd.includes('claude')) return 'claude';
            if (cmd.includes('codex')) return 'codex';

            // Check if it's a node process running claude-related hooks
            if (cmd.includes('node') && (
                cmd.includes('.claude/hooks') ||
                cmd.includes('tmux-') ||
                cmd.includes('investigation-')
            )) {
                return 'claude-hook';
            }

            return 'unknown';
        } catch (e) {
            return 'unknown';
        }
    }

    /**
     * Get display-friendly client type label
     */
    getClientTypeLabel(processType) {
        switch (processType) {
            case 'claude':
                return 'Claude';
            case 'codex':
                return 'Codex';
            case 'claude-hook':
                return 'Claude';
            default:
                return 'Claude'; // Default fallback
        }
    }

    /**
     * Get process command line
     */
    getProcessCommand(pid) {
        try {
            const cmdline = fs.readFileSync(`/proc/${pid}/cmdline`, 'utf8');
            return cmdline.replace(/\0/g, ' ').trim();
        } catch (e) {
            try {
                // Fallback to ps command
                const result = execSync(`ps -p ${pid} -o command= 2>/dev/null`, {
                    encoding: 'utf8',
                    timeout: 1000
                }).trim();
                return result;
            } catch (e2) {
                return null;
            }
        }
    }

    /**
     * Get process TTY
     */
    getProcessTTY(pid) {
        try {
            const tty = execSync(`ps -p ${pid} -o tty= 2>/dev/null`, {
                encoding: 'utf8',
                timeout: 1000
            }).trim();

            if (tty && tty !== '?' && tty !== '??') {
                return tty.startsWith('/dev/') ? tty : `/dev/${tty}`;
            }
        } catch (e) {}
        return null;
    }

    /**
     * Get parent process ID
     */
    getParentPid(pid) {
        try {
            const ppid = execSync(`ps -p ${pid} -o ppid= 2>/dev/null`, {
                encoding: 'utf8',
                timeout: 1000
            }).trim();
            return parseInt(ppid) || null;
        } catch (e) {
            return null;
        }
    }

    /**
     * Check if process is alive
     */
    isProcessAlive(pid) {
        try {
            // Send signal 0 to check if process exists
            process.kill(pid, 0);
            return true;
        } catch (e) {
            return false;
        }
    }

    /**
     * Get registry statistics
     */
    getStats() {
        const registry = this.loadRegistry();
        const activeProcesses = this.getActiveProcesses();

        const stats = {
            total_registered: Object.keys(registry.processes).length,
            active_count: Object.keys(activeProcesses).length,
            by_type: {},
            last_cleanup: registry.metadata.last_cleanup || 0,
            registry_age: Date.now() - (registry.metadata.created || Date.now())
        };

        // Count by type
        for (const process of Object.values(activeProcesses)) {
            stats.by_type[process.type] = (stats.by_type[process.type] || 0) + 1;
        }

        return stats;
    }

    /**
     * Link process to conversation
     */
    linkConversation(pid, conversationId) {
        const registry = this.loadRegistry();
        const pidStr = pid.toString();

        if (registry.processes[pidStr]) {
            registry.processes[pidStr].conversation_id = conversationId;
            registry.processes[pidStr].last_update = Date.now();
            this.saveRegistry(registry);

            this.logActivity('link_conversation', {
                pid: pid,
                conversation_id: conversationId
            });

            return true;
        }

        return false;
    }

    /**
     * Get unified session view combining processes and conversations
     */
    getUnifiedSessions() {
        const activeProcesses = this.getActiveProcesses();
        const sessions = [];

        for (const process of Object.values(activeProcesses)) {
            const session = {
                process_info: process,
                conversation_id: process.conversation_id,
                is_active: true,
                session_type: 'active_cli',
                window: process.window,
                duration: Date.now() - process.start_time,
                last_activity: process.last_heartbeat
            };

            sessions.push(session);
        }

        return sessions.sort((a, b) => b.last_activity - a.last_activity);
    }

    /**
     * Log process registration events
     */
    logRegistration(action, process, context = {}) {
        try {
            const logEntry = {
                timestamp: new Date().toISOString(),
                type: 'process_registry',
                action: action,
                pid: process.pid,
                process_type: process.type,
                conversation_id: process.conversation_id,
                window: process.window,
                context: context
            };

            fs.appendFileSync('/tmp/process-registry.log',
                JSON.stringify(logEntry) + '\n');
        } catch (e) {
            // Silent fail for logging
        }
    }

    /**
     * Log general activity
     */
    logActivity(action, data) {
        try {
            const logEntry = {
                timestamp: new Date().toISOString(),
                type: 'process_registry_activity',
                action: action,
                data: data
            };

            fs.appendFileSync('/tmp/process-registry.log',
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
                type: 'process_registry_error',
                operation: operation,
                error: error.message,
                stack: error.stack
            };

            fs.appendFileSync('/tmp/process-registry.log',
                JSON.stringify(logEntry) + '\n');
        } catch (e) {
            // Silent fail for logging
        }
    }

    /**
     * Auto-register current process if it looks like Claude/Codex
     */
    autoRegisterIfCLI() {
        const pid = process.pid;
        const type = this.detectProcessType(pid);

        if (type === 'claude' || type === 'codex') {
            const existing = this.getProcess(pid);
            if (!existing) {
                this.registerProcess(pid, {
                    type: type,
                    auto_registered: true
                });

                this.logActivity('auto_register', {
                    pid: pid,
                    type: type
                });

                return true;
            }
        }

        return false;
    }
}

// Export singleton
const processRegistry = new ProcessRegistry();

module.exports = {
    ProcessRegistry,
    processRegistry
};