#!/usr/bin/env node

/**
 * Plan Tracker - Capture and track ExitPlanMode events
 *
 * Transforms ephemeral planning into persistent cognitive scaffolding by:
 * - Capturing every plan created via ExitPlanMode
 * - Storing plans in per-conversation databases
 * - Linking plans to storyboard beats
 * - Enabling multi-agent plan coordination
 * - Tracking plan lifecycle
 *
 * Plan Object Schema:
 * {
 *   id: string,                      // Unique plan ID (plan_timestamp_hash)
 *   conversation_id: string,         // Conversation this plan belongs to
 *   session_id: string,              // Session when plan was created
 *   title: string,                   // Extracted from plan content
 *   content: string,                 // Full plan markdown
 *   status: string,                  // Plan status (see below)
 *   created_at: number,              // Timestamp (ms)
 *   approved_at: number|null,        // When user approved (not used yet)
 *   completed_at: number|null,       // When manually marked complete
 *   tags: string[],                  // Extracted technical terms
 *   linked_todos: string[],          // Future: link to TodoWrite items
 *   linked_files: string[],          // File references from plan content
 *   modified_files: string[],        // Files actually edited during plan (via Stop hook)
 *   agent_id: string|null,           // Assigned agent (conversation_id or null)
 *   phases: Array<{                  // Extracted plan phases
 *     number: number,
 *     title: string,
 *     status: 'pending'|'in_progress'|'completed'
 *   }>
 * }
 *
 * Plan Statuses:
 * - pending: Plan created but not started (not used - defaults to in_progress)
 * - in_progress: Auto-assigned to agent when created (default, stays until manual completion)
 * - completed: Manually marked done via TUI (not auto-completed by Stop hook)
 * - abandoned: Work stopped, plan no longer relevant
 * - wont_do: Decided not to pursue this approach
 * - pivoted: Plan evolved into different direction
 *
 * Phase Updates:
 * - Stop hook fuzzy matches completed TodoWrite items to phases
 * - Updates phase status from 'pending' to 'completed'
 * - Tracks modified_files array (accumulates across sessions)
 * - NEVER auto-completes plan - always requires manual TUI action
 */

const fs = require('fs');
const path = require('path');

const startTime = Date.now();
const crypto = require('crypto');
const { conversationTracker } = require('../lib/conversation-tracker');

function main() {
    try {
        // Read tool use input
        let input;
        try {
            const stdinBuffer = fs.readFileSync(0, 'utf8');
            input = JSON.parse(stdinBuffer);
        } catch (e) {
            return;
        }

        // Debug: Log what we received
        fs.appendFileSync('/tmp/plan-tracker-debug.log',
            `${new Date().toISOString()} - Received tool: ${input.tool_name}\n`);

        const toolName = input.tool_name;
        if (toolName !== 'ExitPlanMode') {
            return;
        }

        // Debug: Log full input for ExitPlanMode
        fs.appendFileSync('/tmp/plan-tracker-exitplan.log',
            `${new Date().toISOString()} - ExitPlanMode input:\n${JSON.stringify(input, null, 2)}\n\n`);

        const planContent = input.tool_input?.plan;  // Fixed: tool_input not parameters
        if (!planContent) {
            fs.appendFileSync('/tmp/plan-tracker-exitplan.log',
                `${new Date().toISOString()} - ERROR: No plan content in tool_input\n`);
            return;
        }

        const transcriptPath = input.transcript_path;
        const sessionId = input.session_id;
        const conversationId = conversationTracker.extractConversationId(transcriptPath);

        if (!conversationId) {
            return;
        }

        // Send ONLY to MCP server - let it handle ALL extraction
        const mcpSuccess = sendPlanToMCP({
            conversation_id: conversationId,
            session_id: sessionId,
            agent_id: conversationId
        }, planContent);

        if (!mcpSuccess) {
            fs.appendFileSync('/tmp/plan-tracker.log',
                `${new Date().toISOString()} - ❌ FAILED to send plan to MCP - NO FALLBACK\n`);
        }

    } catch (error) {
        // Silent fail - don't block the tool
        fs.appendFileSync('/tmp/plan-tracker.log',
            `${new Date().toISOString()} - Error: ${error.message}\n${error.stack}\n`);
    }
}

/**
 * Generate unique plan ID
 */
function generatePlanId() {
    const timestamp = Date.now();
    const random = crypto.randomBytes(4).toString('hex');
    return `plan_${timestamp}_${random}`;
}

/**
 * Extract title from plan content (first H2 or meaningful line)
 */
function extractTitle(content) {
    const lines = content.split('\n');

    // Try to find first H2 heading
    for (const line of lines) {
        if (line.startsWith('##')) {
            return line.replace(/^##\s*/, '').trim();
        }
    }

    // Fallback to first non-empty line
    for (const line of lines) {
        const trimmed = line.trim();
        if (trimmed && !trimmed.startsWith('#')) {
            return trimmed.substring(0, 80);
        }
    }

    return 'Untitled Plan';
}

/**
 * Extract tags from plan content
 */
function extractTags(content) {
    const tags = new Set();

    // Extract words that might be tags
    const words = content.toLowerCase().match(/\b[a-z]{3,}\b/g) || [];

    // Common technical terms to tag
    const techTerms = [
        'rendering', 'gpu', 'shader', 'pipeline', 'dgc', 'vulkan',
        'physics', 'collision', 'ui', 'tui', 'chat', 'agent',
        'storyboard', 'plan', 'todo', 'refactor', 'debug',
        'performance', 'optimization', 'memory', 'buffer'
    ];

    for (const term of techTerms) {
        if (words.includes(term)) {
            tags.add(term);
        }
    }

    return Array.from(tags).slice(0, 10); // Limit to 10 tags
}

/**
 * Extract file references from plan content
 */
function extractFileReferences(content) {
    const files = new Set();

    // Match file paths like src/foo.rs, shaders/bar.glsl, etc.
    const filePattern = /(?:^|\s)([a-zA-Z0-9_\-\/]+\.[a-z]{1,5})/g;
    const matches = content.matchAll(filePattern);

    for (const match of matches) {
        files.add(match[1]);
    }

    return Array.from(files).slice(0, 20); // Limit to 20 files
}

/**
 * Extract phases from plan content
 */
function extractPhases(content) {
    const phases = [];
    const lines = content.split('\n');

    let currentPhase = null;
    for (const line of lines) {
        // Match "### Phase X:" or "## Phase X:"
        const phaseMatch = line.match(/^#{2,3}\s*(?:Phase\s+(\d+)|(\d+)\.)[\s:]+(.+)/i);
        if (phaseMatch) {
            if (currentPhase) {
                phases.push(currentPhase);
            }
            currentPhase = {
                number: parseInt(phaseMatch[1] || phaseMatch[2]),
                title: phaseMatch[3].trim(),
                status: 'pending'
            };
        }
    }

    if (currentPhase) {
        phases.push(currentPhase);
    }

    return phases;
}

/**
 * Load plan database for a conversation
 */
function loadPlanDatabase(conversationId) {
    // Use new storage location: .claude/plans/active/{conversation_id}.json
    const dbPath = path.join(process.env.HOME || '/home/eric', '.claude/plans/active', `${conversationId}.json`);

    try {
        if (fs.existsSync(dbPath)) {
            const data = fs.readFileSync(dbPath, 'utf8');
            return JSON.parse(data);
        }
    } catch (e) {
        // Corrupted file - start fresh
    }

    return {
        conversation_id: conversationId,
        plans: [],
        created_at: Date.now(),
        updated_at: Date.now()
    };
}

/**
 * Save plan database
 */
function savePlanDatabase(conversationId, database) {
    // Use new storage location: .claude/plans/active/{conversation_id}.json
    const dbPath = path.join(process.env.HOME || '/home/eric', '.claude/plans/active', `${conversationId}.json`);

    // Ensure directory exists
    const dirPath = path.dirname(dbPath);
    if (!fs.existsSync(dirPath)) {
        fs.mkdirSync(dirPath, { recursive: true });
    }

    database.updated_at = Date.now();
    fs.writeFileSync(dbPath, JSON.stringify(database, null, 2));
}

/**
 * Log plan capture for debugging
 */
function logPlanCapture(conversationId, title) {
    const logPath = '/tmp/plan-tracker.log';
    const message = `${new Date().toISOString()} - Captured plan: "${title}" (${conversationId})\n`;
    fs.appendFileSync(logPath, message);
}

/**
 * Send full plan to MCP server for agent-aware processing
 * Returns true if successful, false otherwise
 */
function sendPlanToMCP(plan, planContent) {
    try {
        const http = require('http');

        const payload = JSON.stringify({
            conversation_id: plan.conversation_id,
            session_id: plan.session_id,
            agent_id: plan.agent_id,
            plan_content: planContent,
            storyboard_beat: null  // Could extract from context if needed
        });

        const options = {
            hostname: '127.0.0.1',
            port: 3001,
            path: '/api/plan/capture',  // Correct endpoint
            method: 'POST',
            headers: {
                'Content-Type': 'application/json',
                'Content-Length': Buffer.byteLength(payload)
            },
            timeout: 1000
        };

        let success = false;

        const req = http.request(options, (res) => {
            if (res.statusCode === 200) {
                success = true;
                fs.appendFileSync('/tmp/plan-tracker.log',
                    `${new Date().toISOString()} - ✅ Plan sent to MCP server\n`);
            }
        });

        req.on('error', (err) => {
            // Silent fail - MCP server might not be running
            fs.appendFileSync('/tmp/plan-tracker.log',
                `${new Date().toISOString()} - ⚠️  MCP server not available: ${err.message}\n`);
        });

        req.on('timeout', () => {
            req.destroy();
        });

        req.write(payload);
        req.end();

        // Give it a moment to complete (synchronous alternative)
        const start = Date.now();
        while (Date.now() - start < 100) {
            // Busy wait for response
        }

        return success;

    } catch (error) {
        fs.appendFileSync('/tmp/plan-tracker.log',
            `${new Date().toISOString()} - ❌ Error sending to MCP: ${error.message}\n`);
        return false;
    }
}

main();

// Log timing
const elapsed = Date.now() - startTime;
try {
    fs.appendFileSync('/tmp/tmux-hook.log',
        `${new Date().toISOString()} - TIMING: plan-tracker.js took ${elapsed}ms\n`);
} catch (e) {}
