#!/usr/bin/env node

/**
 * Agent Heartbeat Hook - Track agent activity for multi-agent coordination
 *
 * Sends tool usage heartbeat to MCP server whenever an agent uses a tool.
 * This enables real-time tracking of what each agent is working on.
 */

const fs = require('fs');
const { conversationTracker } = require('../lib/conversation-tracker');

const startTime = Date.now();

function main() {
    try {
        if (!process.env.TMUX) return;

        // Skip agent heartbeat outside tengine project
        if (!process.cwd().includes('/tengine')) {
            return;
        }

        // Read tool use input
        let input;
        try {
            const stdinBuffer = fs.readFileSync(0, 'utf8');
            input = JSON.parse(stdinBuffer);
        } catch (e) {
            return;
        }

        const toolName = input.tool_name;
        const toolParams = input.parameters;
        const transcriptPath = input.transcript_path;
        const sessionId = input.session_id || null;

        // Skip noisy tools that fire constantly (e.g. Grep spam during code search)
        const skipTools = new Set(['Grep', 'Glob']);
        if (skipTools.has(toolName)) {
            return;
        }

        // Extract conversation ID
        const conversationId = conversationTracker.extractConversationId(transcriptPath);
        if (!conversationId) return;

        // Write conversation ID to window-specific file for bash to source
        try {
            const { execSync } = require('child_process');
            const windowNum = execSync('tmux display-message -p "#I"', {
                encoding: 'utf8',
                timeout: 500
            }).trim();

            const envFile = `/tmp/claude-env-window-${windowNum}.sh`;
            fs.writeFileSync(envFile, `export CLAUDE_CONVERSATION_ID="${conversationId}"\n`);
        } catch (e) {
            // Silent fail - not critical
        }

        // Extract file paths from tool parameters
        const files = extractFilesFromParams(toolName, toolParams);

        // Send heartbeat to MCP server
        sendHeartbeatToMCP(conversationId, toolName, toolParams, files);

        // Check inbox for new messages (AIM - Agent Instant Messaging)
        checkInboxForMessages(conversationId, sessionId);

    } catch (error) {
        fs.appendFileSync('/tmp/tmux-hook.log',
            `${new Date().toISOString()} - AgentHeartbeat: Error ${error.message}\n`);
    }
}

/**
 * Extract file paths from tool parameters based on tool type
 */
function extractFilesFromParams(toolName, params) {
    if (!params) return null;

    const files = [];

    // Read tool - file_path parameter
    if (toolName === 'Read' && params.file_path) {
        files.push(params.file_path);
    }

    // Write tool - file_path parameter
    if (toolName === 'Write' && params.file_path) {
        files.push(params.file_path);
    }

    // Edit tool - file_path parameter
    if (toolName === 'Edit' && params.file_path) {
        files.push(params.file_path);
    }

    // Grep tool - path parameter
    if (toolName === 'Grep' && params.path) {
        files.push(params.path);
    }

    // Glob tool - path parameter
    if (toolName === 'Glob' && params.path) {
        files.push(params.path);
    }

    // MCP tools with file_path
    if (params.file_path && toolName.startsWith('mcp__')) {
        files.push(params.file_path);
    }

    return files.length > 0 ? files : null;
}

/**
 * Send heartbeat to MCP server
 */
function sendHeartbeatToMCP(conversationId, toolName, toolParams, files) {
    try {
        const http = require('http');

        const payload = JSON.stringify({
            conversation_id: conversationId,
            tool_name: toolName,
            tool_params: toolParams,
            files: files
        });

        const options = {
            hostname: '127.0.0.1',
            port: 3001,
            path: '/api/agent/heartbeat',
            method: 'POST',
            headers: {
                'Content-Type': 'application/json',
                'Content-Length': Buffer.byteLength(payload)
            },
            timeout: 500
        };

        const req = http.request(options, (res) => {
            // Silent success - no need to log every heartbeat
        });

        req.on('error', (e) => {
            // Silent fail - MCP server might not be running
        });

        req.on('timeout', () => {
            req.destroy();
        });

        req.write(payload);
        req.end();

    } catch (error) {
        // Silent fail - don't spam logs
    }
}

/**
 * Check inbox for new messages and output to stdout for Claude to see
 */
function checkInboxForMessages(conversationId, sessionId) {
    try {
        const http = require('http');

        const payload = JSON.stringify({
            conversation_id: conversationId
        });

        const options = {
            hostname: '127.0.0.1',
            port: 3001,
            path: '/api/agent/inbox',
            method: 'POST',
            headers: {
                'Content-Type': 'application/json',
                'Content-Length': Buffer.byteLength(payload)
            },
            timeout: 500
        };

        const req = http.request(options, (res) => {
            let data = '';

            res.on('data', (chunk) => {
                data += chunk;
            });

            res.on('end', () => {
                try {
                    const response = JSON.parse(data);
                    const messages = response.messages || [];

                    // Debug log ALL inbox checks
                    fs.appendFileSync('/tmp/agent-heartbeat-inbox.log',
                        `${new Date().toISOString()} - Inbox check: ${messages.length} messages (sessionId=${sessionId})\n`);

                    if (messages.length > 0) {
                        // Queue messages in feedback DB for final collector to send
                        const { getFeedbackDB } = require('../lib/feedback-db');
                        const feedbackDB = getFeedbackDB(sessionId);

                        const messageIds = [];
                        const messageLines = [];

                        for (const msg of messages) {
                            messageIds.push(msg.id);

                            const typeEmoji = getMessageTypeEmoji(msg.message_type);
                            const fromWindow = msg.from_window ? ` (W${msg.from_window})` : '';
                            const timestamp = new Date(msg.timestamp).toLocaleTimeString();

                            // Format message for Claude to see
                            messageLines.push(`📬 ${typeEmoji} Agent Message${fromWindow} at ${timestamp}:`);
                            messageLines.push(`   ${msg.content}`);
                        }

                        // Add to feedback DB - final collector will send with exit code 2
                        const feedbackMessage = '\n' + messageLines.join('\n') + '\n';
                        feedbackDB.addMessage('agent-inbox', feedbackMessage, 'high');

                        // Debug log
                        fs.appendFileSync('/tmp/agent-heartbeat-inbox.log',
                            `${new Date().toISOString()} - Queued ${messages.length} messages to feedback DB (sessionId=${sessionId})\n`);

                        // Mark messages as read
                        markMessagesAsRead(conversationId, messageIds);
                    }
                } catch (e) {
                    // Failed to parse response - silent fail
                }
            });
        });

        req.on('error', (e) => {
            // Silent fail - MCP server might not be running
        });

        req.on('timeout', () => {
            req.destroy();
        });

        req.write(payload);
        req.end();

    } catch (error) {
        // Silent fail - don't spam logs
    }
}

/**
 * Mark messages as read after displaying them
 */
function markMessagesAsRead(conversationId, messageIds) {
    try {
        const http = require('http');

        const payload = JSON.stringify({
            conversation_id: conversationId,
            message_ids: messageIds
        });

        const options = {
            hostname: '127.0.0.1',
            port: 3001,
            path: '/api/agent/inbox/mark-read',
            method: 'POST',
            headers: {
                'Content-Type': 'application/json',
                'Content-Length': Buffer.byteLength(payload)
            },
            timeout: 500
        };

        const req = http.request(options);

        req.on('error', () => {
            // Silent fail
        });

        req.on('timeout', () => {
            req.destroy();
        });

        req.write(payload);
        req.end();

    } catch (error) {
        // Silent fail
    }
}

/**
 * Get emoji for message type
 */
function getMessageTypeEmoji(messageType) {
    const emojiMap = {
        'Info': 'ℹ️',
        'Question': '❓',
        'Request': '🙏',
        'Completion': '✅',
        'System': '🔔'
    };
    return emojiMap[messageType] || 'ℹ️';
}

main();

// Log timing
const elapsed = Date.now() - startTime;
try {
    fs.appendFileSync('/tmp/tmux-hook.log',
        `${new Date().toISOString()} - TIMING: agent-heartbeat.js took ${elapsed}ms\n`);
} catch (e) {
    // Silent fail
}
