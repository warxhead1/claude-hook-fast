#!/usr/bin/env node

/**
 * Conversation Tracker - Smart conversation-centric session management
 *
 * Uses transcript_path as the stable conversation identifier instead of the
 * volatile session_id that changes on every resume. Provides true conversation
 * persistence across resumes, compacts, and window switches.
 */

const fs = require('fs');
const path = require('path');

class ConversationTracker {
    constructor() {
        this.conversationDataFile = '/tmp/claude-conversations.json';
        this.windowMappingFile = '/tmp/claude-window-conversations.json';
    }

    /**
     * Extract conversation ID from transcript path
     * @param {string} transcriptPath - Path like "/home/user/.claude/projects/PROJECT/CONVERSATION_ID.jsonl"
     * @returns {string|null} - The conversation ID or null if invalid
     */
    extractConversationId(transcriptPath) {
        try {
            if (!transcriptPath) return null;

            const filename = path.basename(transcriptPath, '.jsonl');
            // Conversation ID should be a UUID-like string
            if (filename.match(/^[a-f0-9]{8}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{12}$/)) {
                return filename;
            }
            return null;
        } catch (e) {
            return null;
        }
    }

    /**
     * Load all conversation data
     */
    loadConversations() {
        try {
            if (fs.existsSync(this.conversationDataFile)) {
                return JSON.parse(fs.readFileSync(this.conversationDataFile, 'utf8'));
            }
        } catch (e) {}
        return {};
    }

    /**
     * Save conversation data
     */
    saveConversations(conversations) {
        try {
            fs.writeFileSync(this.conversationDataFile, JSON.stringify(conversations, null, 2));
        } catch (e) {
            fs.appendFileSync('/tmp/tmux-hook.log',
                `${new Date().toISOString()} - ConversationTracker: Error saving conversations: ${e.message}\n`);
        }
    }

    /**
     * Load window to conversation mapping
     */
    loadWindowMapping() {
        try {
            if (fs.existsSync(this.windowMappingFile)) {
                return JSON.parse(fs.readFileSync(this.windowMappingFile, 'utf8'));
            }
        } catch (e) {}
        return {};
    }

    /**
     * Save window to conversation mapping
     */
    saveWindowMapping(mapping) {
        try {
            fs.writeFileSync(this.windowMappingFile, JSON.stringify(mapping, null, 2));
        } catch (e) {}
    }

    /**
     * Get conversation data by conversation ID
     */
    getConversation(conversationId) {
        const conversations = this.loadConversations();
        return conversations[conversationId] || null;
    }

    /**
     * Create or update conversation data
     */
    updateConversation(conversationId, windowId, sessionId = null, isNewConversation = false) {
        const conversations = this.loadConversations();
        const windowMapping = this.loadWindowMapping();
        const now = Date.now();

        let tmuxSession = null;
        try {
            const { getSessionManager } = require('./session-manager');
            const sessionManager = getSessionManager();
            const detection = sessionManager && sessionManager.detectSession ? sessionManager.detectSession() : null;
            if (detection && detection.sessionId) {
                tmuxSession = detection.sessionId;
            }
        } catch (e) {
            // Best-effort; session name enriches notifications but is optional
            tmuxSession = null;
        }

        let conversation = conversations[conversationId];

        if (!conversation || isNewConversation) {
            // New conversation
            conversation = {
                conversationId: conversationId,
                conversation_start_time: now,
                current_window: windowId,
                total_operations: 0,
                last_activity: now,
                recent_session_ids: sessionId ? [sessionId] : [],
                windows_used: [windowId],
                created: now,
                last_update: now,
                tmux_session: tmuxSession
            };

            fs.appendFileSync('/tmp/tmux-hook.log',
                `${new Date().toISOString()} - ConversationTracker: NEW conversation ${conversationId} in window ${windowId}\n`);
        } else {
            // Existing conversation - update activity
            conversation.last_activity = now;
            conversation.last_update = now;

            // Update current window if changed
            if (conversation.current_window !== windowId) {
                fs.appendFileSync('/tmp/tmux-hook.log',
                    `${new Date().toISOString()} - ConversationTracker: Conversation ${conversationId} moved from window ${conversation.current_window} to ${windowId}\n`);

                conversation.current_window = windowId;

                // Add to windows_used if not already there
                if (!conversation.windows_used.includes(windowId)) {
                    conversation.windows_used.push(windowId);
                }
            }

            // Track recent session IDs (keep last 5)
            if (sessionId && !conversation.recent_session_ids.includes(sessionId)) {
                conversation.recent_session_ids.unshift(sessionId);
                conversation.recent_session_ids = conversation.recent_session_ids.slice(0, 5);
            }

            if (tmuxSession) {
                conversation.tmux_session = tmuxSession;
            }
        }

        // Save conversation data
        conversations[conversationId] = conversation;
        this.saveConversations(conversations);

        // Update window mapping
        windowMapping[windowId] = conversationId;
        this.saveWindowMapping(windowMapping);

        return conversation;
    }

    /**
     * Find conversation metadata by a recent session identifier
     */
    findConversationBySession(sessionId) {
        if (!sessionId) return null;

        try {
            const conversations = this.loadConversations();
            for (const [conversationId, conversation] of Object.entries(conversations)) {
                const sessions = conversation.recent_session_ids || [];
                if (Array.isArray(sessions) && sessions.includes(sessionId)) {
                    return { conversationId, conversation };
                }
            }
        } catch (e) {
            // Ignore lookup errors and fall through to null
        }

        return null;
    }

    /**
     * Get conversation by window ID
     */
    getConversationByWindow(windowId) {
        const windowMapping = this.loadWindowMapping();
        const conversationId = windowMapping[windowId];

        if (conversationId) {
            return this.getConversation(conversationId);
        }
        return null;
    }

    /**
     * Determine if this is a new conversation based on input data
     */
    isNewConversation(transcriptPath, sessionId, source) {
        const conversationId = this.extractConversationId(transcriptPath);
        if (!conversationId) return true;

        const existing = this.getConversation(conversationId);

        // If no existing conversation data, it's new
        if (!existing) return true;

        // If source is 'clear', it's definitely new/reset
        if (source === 'clear') return true;

        // If source is 'startup', it's likely new
        if (source === 'startup') return true;

        // For 'resume' and 'compact', check if we have recent data
        const timeSinceLastActivity = Date.now() - existing.last_activity;
        const maxAge = 24 * 60 * 60 * 1000; // 24 hours

        // If last activity was more than 24 hours ago, treat as new
        if (timeSinceLastActivity > maxAge) {
            return true;
        }

        return false;
    }

    /**
     * Increment operation count for a conversation
     */
    incrementOperations(conversationId, windowId) {
        const conversation = this.getConversation(conversationId);
        if (conversation) {
            conversation.total_operations++;
            conversation.last_activity = Date.now();
            conversation.last_update = Date.now();

            // Update current window
            if (conversation.current_window !== windowId) {
                conversation.current_window = windowId;
            }

            // Save
            const conversations = this.loadConversations();
            conversations[conversationId] = conversation;
            this.saveConversations(conversations);

            return conversation.total_operations;
        }
        return 0;
    }

    /**
     * Resolve actual window for a conversation using TTY detection
     * Returns null if conversation can't be located
     */
    resolveActualWindow(conversationId) {
        try {
            const { execSync } = require('child_process');
            const { getWindowId } = require('./session-manager');

            // Get current actual window from TTY resolution
            const actualWindow = getWindowId();

            // Get stored window for this conversation
            const conversation = this.getConversation(conversationId);
            const storedWindow = conversation?.current_window;

            // If no stored window or mismatch detected, update mappings
            if (!storedWindow || storedWindow !== actualWindow) {
                this.updateWindowMapping(conversationId, actualWindow, storedWindow);
                return actualWindow;
            }

            return storedWindow;
        } catch (e) {
            fs.appendFileSync('/tmp/tmux-hook.log',
                `${new Date().toISOString()} - ConversationTracker: Failed to resolve window for ${conversationId}: ${e.message}\n`);
            return null;
        }
    }

    /**
     * Update window mapping when renumbering detected
     */
    updateWindowMapping(conversationId, newWindow, oldWindow) {
        if (!conversationId || !newWindow) return;

        const conversations = this.loadConversations();
        const windowMapping = this.loadWindowMapping();

        // Update conversation data
        if (conversations[conversationId]) {
            conversations[conversationId].current_window = newWindow;
            conversations[conversationId].last_update = Date.now();

            // Track window change
            if (!conversations[conversationId].windows_used.includes(newWindow)) {
                conversations[conversationId].windows_used.push(newWindow);
            }
        }

        // Update window mapping
        windowMapping[newWindow] = conversationId;

        // Clean old mapping if window changed
        if (oldWindow && oldWindow !== newWindow) {
            delete windowMapping[oldWindow];
        }

        // Save both files
        this.saveConversations(conversations);
        this.saveWindowMapping(windowMapping);

        fs.appendFileSync('/tmp/tmux-hook.log',
            `${new Date().toISOString()} - ConversationTracker: Updated mapping ${conversationId}: ${oldWindow || 'none'} → ${newWindow}\n`);
    }

    /**
     * Find conversation by conversation ID, with window resolution fallback
     */
    findConversationRobust(conversationId, windowId = null) {
        // Primary: Use conversation ID
        let conversation = this.getConversation(conversationId);

        if (!conversation && windowId) {
            // Fallback: Try window mapping
            conversation = this.getConversationByWindow(windowId);
        }

        if (conversation && conversationId && conversation.conversationId !== conversationId) {
            // Window mapping points to different conversation - update it
            this.updateWindowMapping(conversationId, windowId, null);
        }

        return conversation;
    }

    /**
     * Get session duration for a conversation
     */
    getSessionDuration(conversationId) {
        const conversation = this.getConversation(conversationId);
        if (conversation) {
            const elapsed = Date.now() - conversation.conversation_start_time;
            return this.formatDuration(elapsed);
        }
        return '0s';
    }

    /**
     * Format duration in a human-readable way
     */
    formatDuration(milliseconds) {
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

    /**
     * Get conversation statistics for display
     */
    getConversationStats(conversationId) {
        const conversation = this.getConversation(conversationId);
        if (!conversation) return null;

        return {
            conversationId: conversationId,
            duration: this.getSessionDuration(conversationId),
            operations: conversation.total_operations,
            currentWindow: conversation.current_window,
            windowsUsed: conversation.windows_used,
            lastActivity: new Date(conversation.last_activity).toISOString(),
            recentSessions: conversation.recent_session_ids.length
        };
    }

    /**
     * Update window-specific session stats file for backwards compatibility
     */
    updateWindowStats(conversationId, windowId) {
        const conversation = this.getConversation(conversationId);
        if (!conversation) return;

        try {
            const windowStatsFile = `/tmp/tmux-session-stats-${windowId}.json`;
            const windowStats = {
                startTime: conversation.conversation_start_time,
                operations: conversation.total_operations,
                lastUpdate: Date.now(),
                windowId: windowId,
                conversationId: conversationId,
                sessionId: conversation.recent_session_ids[0] || null
            };

            fs.writeFileSync(windowStatsFile, JSON.stringify(windowStats, null, 2));
        } catch (e) {
            fs.appendFileSync('/tmp/tmux-hook.log',
                `${new Date().toISOString()} - ConversationTracker: Error updating window stats: ${e.message}\n`);
        }
    }

    /**
     * Clean up old conversations (older than 7 days) and their TODO databases
     */
    cleanup() {
        try {
            const conversations = this.loadConversations();
            const windowMapping = this.loadWindowMapping();
            const maxAge = 7 * 24 * 60 * 60 * 1000; // 7 days
            const now = Date.now();
            let cleaned = 0;
            let todoDbsCleaned = 0;

            // Collect conversation IDs to clean up
            const conversationsToClean = [];
            for (const [conversationId, conversation] of Object.entries(conversations)) {
                if (now - conversation.last_activity > maxAge) {
                    conversationsToClean.push(conversationId);
                    delete conversations[conversationId];
                    cleaned++;
                }
            }

            // Clean up window mappings for deleted conversations
            for (const [windowId, conversationId] of Object.entries(windowMapping)) {
                if (!conversations[conversationId]) {
                    delete windowMapping[windowId];
                }
            }

            // Clean up TODO databases for old conversations
            for (const conversationId of conversationsToClean) {
                try {
                    const todoDbPath = `/tmp/todos-${conversationId}.json`;
                    if (fs.existsSync(todoDbPath)) {
                        fs.unlinkSync(todoDbPath);
                        todoDbsCleaned++;
                    }
                } catch (e) {
                    fs.appendFileSync('/tmp/tmux-hook.log',
                        `${new Date().toISOString()} - ConversationTracker: Failed to cleanup TODO DB for ${conversationId}: ${e.message}\n`);
                }
            }

            if (cleaned > 0) {
                this.saveConversations(conversations);
                this.saveWindowMapping(windowMapping);

                fs.appendFileSync('/tmp/tmux-hook.log',
                    `${new Date().toISOString()} - ConversationTracker: Cleaned up ${cleaned} old conversations and ${todoDbsCleaned} TODO databases\n`);
            }

            return { conversations: cleaned, todoDbs: todoDbsCleaned };
        } catch (e) {
            fs.appendFileSync('/tmp/tmux-hook.log',
                `${new Date().toISOString()} - ConversationTracker: Cleanup error: ${e.message}\n`);
            return { conversations: 0, todoDbs: 0 };
        }
    }

    /**
     * Get TODO database path for a conversation
     */
    getTodoDbPath(conversationId) {
        return `/tmp/todos-${conversationId}.json`;
    }

    /**
     * Check if a conversation has an associated TODO database
     */
    hasTodoDatabase(conversationId) {
        try {
            return fs.existsSync(this.getTodoDbPath(conversationId));
        } catch (e) {
            return false;
        }
    }

    /**
     * Get all active conversations
     */
    getAllConversations() {
        return this.loadConversations();
    }

    /**
     * Get window mapping
     */
    getWindowMapping() {
        return this.loadWindowMapping();
    }

    /**
     * Get process registry (lazy loaded to avoid circular dependency)
     */
    getProcessRegistry() {
        if (!this._processRegistry) {
            try {
                const { processRegistry } = require('./process-registry');
                this._processRegistry = processRegistry;
            } catch (e) {
                // Fallback if process registry not available
                this._processRegistry = null;
            }
        }
        return this._processRegistry;
    }

    /**
     * Determine conversation state based on process registry
     * @param {string} conversationId - The conversation ID
     * @returns {string} - 'active', 'suspended', or 'historical'
     */
    getConversationState(conversationId) {
        const processRegistry = this.getProcessRegistry();
        if (!processRegistry) {
            // Fallback to basic time-based determination
            const conversation = this.getConversation(conversationId);
            if (!conversation) return 'historical';

            const timeSinceActivity = Date.now() - conversation.last_activity;
            const hoursSinceActivity = timeSinceActivity / (1000 * 60 * 60);

            if (hoursSinceActivity < 1) return 'active';
            if (hoursSinceActivity < 24) return 'suspended';
            return 'historical';
        }

        // Check if conversation has an active process
        const activeProcess = processRegistry.getProcessByConversation(conversationId);
        if (activeProcess) {
            return 'active';
        }

        // Check for recent activity to distinguish suspended vs historical
        const conversation = this.getConversation(conversationId);
        if (!conversation) return 'historical';

        const timeSinceActivity = Date.now() - conversation.last_activity;
        const hoursSinceActivity = timeSinceActivity / (1000 * 60 * 60);

        // If recent activity but no process, it's suspended
        if (hoursSinceActivity < 24) {
            return 'suspended';
        }

        return 'historical';
    }

    /**
     * Get conversations by state
     * @param {string} state - 'active', 'suspended', 'historical', or 'all'
     * @returns {Object} - Conversations filtered by state
     */
    getConversationsByState(state = 'all') {
        const allConversations = this.loadConversations();

        if (state === 'all') {
            return allConversations;
        }

        const filtered = {};
        for (const [conversationId, conversation] of Object.entries(allConversations)) {
            const conversationState = this.getConversationState(conversationId);
            if (conversationState === state) {
                filtered[conversationId] = {
                    ...conversation,
                    state: conversationState
                };
            }
        }

        return filtered;
    }

    /**
     * Get active conversations with process information
     * @returns {Array} - Array of active conversation objects with process info
     */
    getActiveConversations() {
        const processRegistry = this.getProcessRegistry();
        const activeConversations = this.getConversationsByState('active');
        const result = [];

        for (const [conversationId, conversation] of Object.entries(activeConversations)) {
            const process = processRegistry?.getProcessByConversation(conversationId);

            result.push({
                ...conversation,
                state: 'active',
                process_info: process || null,
                has_active_process: !!process
            });
        }

        // Sort by last activity (most recent first)
        return result.sort((a, b) => b.last_activity - a.last_activity);
    }

    /**
     * Get conversation summary with state information
     * @returns {Object} - Summary of conversations by state
     */
    getConversationSummary() {
        const processRegistry = this.getProcessRegistry();
        const allConversations = this.loadConversations();
        const stats = {
            total: Object.keys(allConversations).length,
            active: 0,
            suspended: 0,
            historical: 0,
            with_processes: 0,
            conversations_by_state: {
                active: {},
                suspended: {},
                historical: {}
            }
        };

        for (const [conversationId, conversation] of Object.entries(allConversations)) {
            const state = this.getConversationState(conversationId);
            const process = processRegistry?.getProcessByConversation(conversationId);

            stats[state]++;
            if (process) stats.with_processes++;

            stats.conversations_by_state[state][conversationId] = {
                ...conversation,
                state: state,
                process_info: process || null
            };
        }

        return stats;
    }

    /**
     * Update conversation state when process status changes
     * @param {string} conversationId - The conversation ID
     * @param {string} newState - New state ('active', 'suspended', 'historical')
     */
    updateConversationState(conversationId, newState) {
        const conversations = this.loadConversations();
        const conversation = conversations[conversationId];

        if (conversation) {
            conversation.state = newState;
            conversation.last_state_change = Date.now();

            this.saveConversations(conversations);

            try {
                fs.appendFileSync('/tmp/tmux-hook.log',
                    `${new Date().toISOString()} - ConversationTracker: State changed for ${conversationId}: ${newState}\n`);
            } catch (e) {
                // Silent fail for logging
            }
        }
    }
}

// Export singleton
const conversationTracker = new ConversationTracker();

module.exports = {
    ConversationTracker,
    conversationTracker
};
