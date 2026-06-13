#!/usr/bin/env node

/**
 * Feedback Collector - Final hook that sends all accumulated messages
 *
 * Runs LAST in the hook chain. Collects all messages from feedback DB
 * and sends them to Claude in one go. Only this hook does process.exit(2).
 */

const { getFeedbackDB } = require('../lib/feedback-db');
const { HookLock } = require('../lib/hook-lock');
const { conversationTracker } = require('../lib/conversation-tracker');
const { processRegistry } = require('../lib/process-registry');
const fs = require('fs');
const path = require('path');
const os = require('os');

const DEBUG_LOG = '/tmp/feedback-collector-debug.log';
function debugLog(message) {
    try {
        fs.appendFileSync(DEBUG_LOG, `${new Date().toISOString()} ${message}\n`);
    } catch (e) {
        // ignore logging failures
    }
}

// Sentiment analysis cache (30s TTL, invalidate on operation count change)
let sentimentCache = null;
let sentimentCacheTimestamp = 0;
let sentimentCacheOperationCount = 0;
const SENTIMENT_CACHE_TTL = 30000; // 30 seconds

// Sentiment decision write batching (flush every 10 decisions or on exit)
let sentimentDecisionBuffer = [];
const SENTIMENT_DECISION_FLUSH_THRESHOLD = 10;

async function main() {
    try {
        // Extract session and conversation data from stdin for unified tracking
        let sessionId = null;
        let conversationId = null;
        let transcriptPath = null;
        try {
            let input = '';
            if (!process.stdin.isTTY) {
                const chunks = [];
                for await (const chunk of process.stdin) {
                    chunks.push(chunk);
                }
                input = Buffer.concat(chunks).toString();

                if (input.trim()) {
                    const hookData = JSON.parse(input);
                    sessionId = hookData.session_id;
                    transcriptPath = hookData.transcript_path;

                    // Extract conversation ID from transcript path
                    conversationId = conversationTracker.extractConversationId(transcriptPath);

                    // Fast-exit: non-shader edits never produce shader validation feedback.
                    // Skip the quiescence wait entirely — saves 100-230ms on every Rust/Python/JSON edit.
                    const SHADER_EXTENSIONS = /\.(slang|glsl|hlsl|wgsl|vert|frag|comp|geom|tesc|tese|task|mesh|rgen|rint|rahit|rchit|rmiss|rcall)$/i;
                    const filePath = hookData.tool_input?.file_path || hookData.tool_input?.path || '';
                    const toolName = hookData.tool_name || '';
                    const isShaderEdit = ['Write', 'Edit', 'MultiEdit'].includes(toolName) && SHADER_EXTENSIONS.test(filePath);
                    if (!isShaderEdit && ['Write', 'Edit', 'MultiEdit'].includes(toolName)) {
                        // Not a shader file — check DB quickly for any pre-existing messages, then exit
                        const primaryKey = conversationTracker.extractConversationId(transcriptPath) || sessionId;
                        if (primaryKey) {
                            const fastDB = getFeedbackDB(primaryKey);
                            const fastMessages = fastDB.getAndClearMessages();
                            if (fastMessages.length === 0) {
                                console.log(JSON.stringify({ continue: true }));
                                logTimingAndExit(0);
                                return;
                            }
                        } else {
                            console.log(JSON.stringify({ continue: true }));
                            logTimingAndExit(0);
                            return;
                        }
                    }
                }
            }
        } catch (e) {
            // Fall back to non-session-aware if parsing fails
        }

        mark('stdin_read');
        debugLog(`collector_start session=${sessionId || 'null'} conversation=${conversationId || 'null'}`);

        // Create conversation context for unified feedback tracking
        const feedbackContext = {
            session_id: sessionId,
            conversation_id: conversationId,
            transcript_path: transcriptPath,
            process_info: null,
            conversation_state: null,
            window_id: null
        };

        // Enhance context with process and conversation information
        if (conversationId) {
            try {
                // Get conversation state
                feedbackContext.conversation_state = conversationTracker.getConversationState(conversationId);

                // Get process information if available
                const process = processRegistry.getProcessByConversation(conversationId);
                if (process) {
                    feedbackContext.process_info = {
                        pid: process.pid,
                        type: process.type,
                        window: process.window,
                        operation_count: process.operation_count
                    };
                    feedbackContext.window_id = process.window;
                }

                // Get conversation data
                const conversation = conversationTracker.getConversation(conversationId);
                if (conversation) {
                    feedbackContext.conversation_duration = Date.now() - conversation.conversation_start_time;
                    feedbackContext.total_operations = conversation.total_operations;
                    feedbackContext.window_id = feedbackContext.window_id || conversation.current_window;
                }
            } catch (e) {
                // Continue with basic context if enhanced context fails
            }
        }

        mark('context_built');

        // DEBUG: Log enhanced session coordination details
        try {
            fs.appendFileSync('/tmp/hook-session-debug.log',
                `${new Date().toISOString()} - FEEDBACK-COLLECTOR: session="${sessionId || 'null'}" conversation="${conversationId || 'null'}" state="${feedbackContext.conversation_state || 'unknown'}" window="${feedbackContext.window_id || 'unknown'}"\n`);
        } catch (e) {}

        // Use conversation-aware feedback DB (fall back to session-based if no conversation ID)
        const primaryKey = conversationId || sessionId;
        const sessionFeedbackDB = getFeedbackDB(primaryKey);

        const quiesced = await waitForHookQuiescence(primaryKey, sessionFeedbackDB);
        mark('quiescence_wait');
        if (!quiesced) {
            console.error(`[FEEDBACK-COLLECTOR] Warning: Hooks or feedback writes may still be active in ${conversationId ? `conversation ${conversationId}` : `session ${sessionId}`}`);
            debugLog(`collector_timeout session=${sessionId || 'null'} conversation=${conversationId || 'null'}`);
        }

        // Check for unread agent messages and inject into feedback DB
        // Check both transcript-derived ID and CLAUDE_CONVERSATION_ID env var
        const claudeConvId = process.env.CLAUDE_CONVERSATION_ID;
        const idsToCheck = [...new Set([conversationId, claudeConvId].filter(Boolean))];

        for (const checkId of idsToCheck) {
            try {
                const messageFile = `/tmp/agent-messages-${checkId}.json`;
                if (fs.existsSync(messageFile)) {
                    const messageContent = fs.readFileSync(messageFile, 'utf8');
                    const messageBox = JSON.parse(messageContent);
                    const unreadMessages = messageBox.messages.filter(m => !m.read);

                    for (const msg of unreadMessages) {
                        const typeIcon = {
                            'help_request': '🆘',
                            'status_update': 'ℹ️',
                            'handoff': '🔄',
                            'general': '💬',
                            'system_event': '⚡'
                        }[msg.message_type] || '📬';

                        const timestamp = new Date(msg.timestamp);
                        const timeStr = timestamp.toLocaleTimeString('en-US', {
                            hour: 'numeric',
                            minute: '2-digit',
                            second: '2-digit',
                            hour12: true
                        });

                        const fromAgent = msg.from_conversation_id?.substring(0, 8) || 'brain';
                        const messageText = `📬 ${typeIcon} Agent Message (${fromAgent}) at ${timeStr}:\n   ${msg.content}`;

                        sessionFeedbackDB.addMessage('agent-message', messageText, 'normal');
                        msg.read = true;
                    }
                    // Write back with messages marked as read
                    fs.writeFileSync(messageFile, JSON.stringify(messageBox, null, 2));
                }
            } catch (e) {
                // Ignore agent message errors
            }
        }

        mark('agent_messages_checked');

        // Get all accumulated messages (including those from THIS hook chain)
        const messages = sessionFeedbackDB.getAndClearMessages();
        mark('messages_retrieved');
        debugLog(`collector_messages session=${sessionId || 'null'} conversation=${conversationId || 'null'} count=${messages.length}`);

        // Analyze sentiment and timing before processing messages
        const sentimentAnalysis = analyzeSentimentFromContext();
        const timingRecommendation = getMessageTimingRecommendation(sentimentAnalysis);
        mark('sentiment_analyzed');

        // Handle achievement triggers first
        const achievementTriggers = messages.filter(m => m.priority === 'achievement-trigger');
        if (achievementTriggers.length > 0) {
            const { qolManager } = require('../lib/qol-manager');
            for (const trigger of achievementTriggers) {
                qolManager.checkAchievements({ toolName: trigger.message, isSuccess: true });
            }
        }

        // Filter out internal signals — sentinels and achievement triggers are never displayed
        let displayMessages = messages.filter(m => m.priority !== 'achievement-trigger' && m.priority !== 'sentinel');

        // Enhance messages with conversation context for better analysis
        displayMessages = displayMessages.map(message => ({
            ...message,
            conversation_context: feedbackContext,
            enhanced_at: Date.now()
        }));

        // Apply sentiment-based filtering
        displayMessages = applySentimentFiltering(displayMessages, sentimentAnalysis, timingRecommendation);
        mark('messages_filtered');

        if (displayMessages.length === 0) {
            // No messages - output proper JSON response
            console.log(JSON.stringify({ continue: true }));
            logTimingAndExit(0);
        }

        // Sort by priority (achievements last, errors first)
        displayMessages.sort((a, b) => {
            const priorities = { 'error': 0, 'warning': 1, 'normal': 2, 'achievement': 3 };
            return (priorities[a.priority] || 2) - (priorities[b.priority] || 2);
        });

        // Format consolidated output for stderr - message already has emoji prefix
        const output = displayMessages.map(msg => msg.message).join('\n');
        mark('output_formatted');

        // Determine exit code based on message severity
        // Only actual 'error' messages should block (exit 2)
        // Warnings and normal messages exit with 0 or 1 (non-blocking)
        const hasBlockingErrors = displayMessages.some(m => m.priority === 'error');
        const exitCode = hasBlockingErrors ? 2 : 0;

        // Send feedback to Claude via exit code
        // stderr = message for Claude, stdout = JSON for hook protocol
        console.error(output);
        logTimingAndExit(exitCode);

    } catch (error) {
        // Silent fail - don't interrupt Claude
        logTimingAndExit(0);
    }
}

/**
 * Analyze sentiment from environmental context and recent patterns
 */
function analyzeSentimentFromContext() {
    try {
        // Get environmental context from QoL manager
        const { qolManager } = require('../lib/qol-manager');
        const currentOperationCount = qolManager.session.operations.length;

        // Check cache validity (30s TTL and operation count unchanged)
        const now = Date.now();
        if (sentimentCache &&
            (now - sentimentCacheTimestamp) < SENTIMENT_CACHE_TTL &&
            currentOperationCount === sentimentCacheOperationCount) {
            return sentimentCache; // Return cached result
        }

        const envContext = qolManager.getEnvironmentalContext();

        // Analyze recent operations for patterns
        const recentOps = qolManager.session.operations.slice(-5);
        const errorRate = recentOps.filter(op => !op.isSuccess).length / Math.max(recentOps.length, 1);

        // Check for frustration indicators
        let frustrationLevel = 0;

        // High error rate indicates frustration
        if (errorRate > 0.6) frustrationLevel += 3;
        else if (errorRate > 0.3) frustrationLevel += 1;

        // Poor environmental conditions
        if (envContext.productivity_score < 40) frustrationLevel += 2;
        if (envContext.focus_state === 'disrupted') frustrationLevel += 2;

        // Rapid tool switching (context switching)
        const toolSwitches = countToolSwitches(recentOps);
        if (toolSwitches > 3) frustrationLevel += 1;

        // Scope expansion detection (look for pattern in recent operations)
        const scopeExpansionRisk = detectScopeExpansion(recentOps);

        const analysis = {
            frustration_level: Math.min(frustrationLevel, 10),
            error_rate: errorRate,
            tool_switches: toolSwitches,
            scope_expansion_risk: scopeExpansionRisk,
            environmental_score: envContext.productivity_score,
            focus_state: envContext.focus_state,
            timestamp: Date.now()
        };

        // Cache the result
        sentimentCache = analysis;
        sentimentCacheTimestamp = Date.now();
        sentimentCacheOperationCount = currentOperationCount;

        return analysis;
    } catch (e) {
        // Return neutral sentiment if analysis fails (don't cache failures)
        return {
            frustration_level: 0,
            error_rate: 0,
            tool_switches: 0,
            scope_expansion_risk: 0,
            environmental_score: 50,
            focus_state: 'unknown',
            timestamp: Date.now()
        };
    }
}

/**
 * Get message timing recommendation based on sentiment
 */
function getMessageTimingRecommendation(sentimentAnalysis) {
    const recommendation = {
        should_display: true,
        delay_seconds: 0,
        filter_achievements: false,
        priority_filter: null,
        reason: 'normal_conditions'
    };

    // High frustration - delay or skip messages
    if (sentimentAnalysis.frustration_level > 6) {
        recommendation.should_display = false;
        recommendation.reason = 'high_frustration_detected';
        return recommendation;
    }

    // Moderate frustration - filter achievements but allow errors/warnings
    if (sentimentAnalysis.frustration_level > 3) {
        recommendation.filter_achievements = true;
        recommendation.reason = 'moderate_frustration_filter_celebrations';
    }

    // Poor focus - delay messages briefly
    if (sentimentAnalysis.focus_state === 'disrupted') {
        recommendation.delay_seconds = 5;
        recommendation.reason = 'disrupted_focus_brief_delay';
    }

    // High scope expansion risk - prioritize warnings
    if (sentimentAnalysis.scope_expansion_risk > 5) {
        recommendation.priority_filter = 'warning_and_above';
        recommendation.reason = 'scope_expansion_focus_on_warnings';
    }

    return recommendation;
}

/**
 * Apply sentiment-based filtering to messages
 */
function applySentimentFiltering(messages, sentimentAnalysis, timingRecommendation) {
    if (!timingRecommendation.should_display) {
        // Record that we're skipping messages due to sentiment
        recordSentimentDecision('skip_all_messages', sentimentAnalysis);
        return [];
    }

    let filteredMessages = messages;

    // Filter achievements if recommended
    if (timingRecommendation.filter_achievements) {
        filteredMessages = filteredMessages.filter(msg =>
            msg.priority !== 'achievement' && msg.source !== 'achievement'
        );
        recordSentimentDecision('filter_achievements', sentimentAnalysis);
    }

    // Apply priority filtering
    if (timingRecommendation.priority_filter === 'warning_and_above') {
        filteredMessages = filteredMessages.filter(msg =>
            ['error', 'warning'].includes(msg.priority)
        );
        recordSentimentDecision('priority_filter_warnings', sentimentAnalysis);
    }

    return filteredMessages;
}

/**
 * Count tool switches in recent operations
 */
function countToolSwitches(recentOps) {
    if (recentOps.length < 2) return 0;

    let switches = 0;
    for (let i = 1; i < recentOps.length; i++) {
        if (recentOps[i].toolName !== recentOps[i-1].toolName) {
            switches++;
        }
    }
    return switches;
}

/**
 * Detect scope expansion patterns in operations
 */
function detectScopeExpansion(recentOps) {
    let expansionRisk = 0;

    // Multiple different file types being edited rapidly
    const uniqueFiles = new Set();
    for (const op of recentOps) {
        if (op.toolInput.file_path) {
            uniqueFiles.add(op.toolInput.file_path);
        }
    }

    if (uniqueFiles.size > 3) expansionRisk += 2;

    // Rapid switching between different tool types
    const toolTypes = recentOps.map(op => op.toolName);
    const uniqueTools = new Set(toolTypes);

    if (uniqueTools.size > 4) expansionRisk += 2;

    // Recent errors suggest complexity overwhelm
    const recentErrors = recentOps.filter(op => !op.isSuccess).length;
    if (recentErrors > 2) expansionRisk += 1;

    return Math.min(expansionRisk, 10);
}

/**
 * Record sentiment-based decisions for learning (batched)
 */
function recordSentimentDecision(decision, sentimentAnalysis) {
    try {
        const logEntry = {
            timestamp: Date.now(),
            decision: decision,
            sentiment_state: sentimentAnalysis,
            hook: 'final-feedback-collector'
        };

        // Buffer decision instead of writing immediately
        sentimentDecisionBuffer.push(logEntry);

        // Flush when buffer reaches threshold
        if (sentimentDecisionBuffer.length >= SENTIMENT_DECISION_FLUSH_THRESHOLD) {
            flushSentimentDecisions();
        }
    } catch (e) {
        // Silent fail - don't break hooks
    }
}

/**
 * Flush batched sentiment decisions to disk
 */
function flushSentimentDecisions() {
    if (sentimentDecisionBuffer.length === 0) return;

    try {
        const logFile = path.join(os.homedir(), '.claude-achievements', 'sentiment-decisions.json');
        const dir = path.dirname(logFile);

        if (!fs.existsSync(dir)) {
            fs.mkdirSync(dir, { recursive: true });
        }

        let decisions = [];
        if (fs.existsSync(logFile)) {
            try {
                decisions = JSON.parse(fs.readFileSync(logFile, 'utf8'));
            } catch (e) {
                decisions = [];
            }
        }

        // Append buffered decisions
        decisions.push(...sentimentDecisionBuffer);

        // Keep only last 1000 decisions
        if (decisions.length > 1000) {
            decisions = decisions.slice(-1000);
        }

        fs.writeFileSync(logFile, JSON.stringify(decisions, null, 2));

        // Clear buffer after successful flush
        sentimentDecisionBuffer = [];
    } catch (e) {
        // Silent fail - don't break hooks
    }
}

const startTime = Date.now();
const timingMarkers = {};

// Helper to add timing markers
function mark(name) {
  const now = Date.now();
  const elapsed = now - startTime;
  timingMarkers[name] = elapsed;
  return elapsed;
}

// Helper function to log timing before exits
function logTimingAndExit(exitCode) {
  const elapsed = Date.now() - startTime;

  // Flush any pending sentiment decisions before exit
  flushSentimentDecisions();

  try {
    // Log overall timing
    fs.appendFileSync('/tmp/tmux-hook.log',
      `${new Date().toISOString()} - TIMING: zzzz-final-feedback-collector.js took ${elapsed}ms\n`);

    // Log detailed timing breakdown if we have markers
    if (Object.keys(timingMarkers).length > 0) {
      const details = Object.entries(timingMarkers)
        .map(([name, time]) => `${name}=${time}ms`)
        .join(' ');
      fs.appendFileSync('/tmp/tmux-hook.log',
        `${new Date().toISOString()} - TIMING-DETAIL: final-feedback-collector: ${details}\n`);
    }
  } catch (e) {
    // Silent fail on logging error
  }
  process.exit(exitCode);
}

main().catch((error) => {
  console.error(error);
  logTimingAndExit(0);
});

async function waitForHookQuiescence(sessionId, sessionFeedbackDB, timeoutMs = 1500, idleMs = 50) {
    const start = Date.now();
    let lastMTime = null;
    let lastChange = Date.now();
    const lockDir = sessionId ? `/tmp/claude-hook-locks-${sessionId}` : '/tmp/claude-hook-locks';
    debugLog(`quiescence_wait_start session=${sessionId || 'null'} lockDir=${lockDir} dbFile=${sessionFeedbackDB.dbFile}`);
    let lastActiveLog = 0;
    let consecutiveQuietChecks = 0;
    const DEBUG_MODE = process.env.CLAUDE_HOOK_DEBUG === '1';

    while (Date.now() - start < timeoutMs) {
        try {
            HookLock.cleanStale(sessionId);
        } catch (e) {
            // ignore cleanup errors
        }

        const activeLocks = HookLock.getActive(sessionId);

        // DEBUG: Log what locks we're finding (only in debug mode)
        if (DEBUG_MODE) {
            try {
                const lockDetails = activeLocks.map(lock => `${lock.hook}:${lock.pid}:${lock.timestamp}`).join(', ');
                fs.appendFileSync('/tmp/hook-session-debug.log',
                    `${new Date().toISOString()} - FEEDBACK-COLLECTOR: HookLock.getActive("${sessionId}") found ${activeLocks.length} locks: [${lockDetails}]\n`);
            } catch (e) {}
        }

        if (activeLocks.length === 0) {
            // Sentinel fast-exit: if validator wrote a completion marker, we're done immediately.
            // No need to wait for mtime stability — the sentinel IS the signal.
            try {
                const messages = sessionFeedbackDB.getMessages();
                if (messages && messages.some(m => m.priority === 'sentinel' && m.source === 'shader-validator')) {
                    debugLog(`quiescence_wait_done session=${sessionId || 'null'} sentinelExit=true`);
                    return true;
                }
            } catch (e) {}

            let currentMTime = null;
            try {
                const stats = fs.statSync(sessionFeedbackDB.dbFile);
                currentMTime = stats.mtimeMs;
            } catch (e) {
                currentMTime = null;
            }

            if (currentMTime !== lastMTime) {
                lastMTime = currentMTime;
                lastChange = Date.now();
                consecutiveQuietChecks = 0; // Reset consecutive counter
            } else {
                consecutiveQuietChecks++; // No change detected
            }

            // Smart early exit: if no locks AND no changes for 2 consecutive checks
            if (consecutiveQuietChecks >= 2 && Date.now() - lastChange >= idleMs) {
                debugLog(`quiescence_wait_done session=${sessionId || 'null'} idleFor=${Date.now() - lastChange} earlyExit=true`);
                return true;
            }

            if (Date.now() - lastChange >= idleMs) {
                debugLog(`quiescence_wait_done session=${sessionId || 'null'} idleFor=${Date.now() - lastChange}`);
                return true;
            }
        } else {
            consecutiveQuietChecks = 0; // Reset counter when locks are active
            const now = Date.now();
            if (now - lastActiveLog > 250 && DEBUG_MODE) {
                debugLog(`quiescence_active_locks session=${sessionId || 'null'} count=${activeLocks.length}`);
                lastActiveLog = now;
            }
            lastChange = Date.now();
            try {
                const stats = fs.statSync(sessionFeedbackDB.dbFile);
                lastMTime = stats.mtimeMs;
            } catch (e) {
                lastMTime = null;
            }
        }

        // Increased polling interval: 50ms → 100ms (50% reduction in iterations)
        await new Promise(resolve => setTimeout(resolve, 100));
    }

    return false;
}
