#!/usr/bin/env node

/**
 * Unified Session Manager - Central session and window identification for Claude Code
 *
 * Provides:
 * - Robust tmux window/pane detection with fallbacks
 * - Unified session ID generation for state isolation
 * - Per-window/pane state storage with automatic isolation
 * - Backwards compatibility with existing state files
 * - Centralized session context for all hooks
 */

const { execSync } = require('child_process');
const fs = require('fs');
const path = require('path');

class SessionManager {
    constructor() {
        this.sessionId = null;
        this.windowId = null;
        this.paneId = null;
        this.processInfo = null;

        // Cache for performance
        this.detectionCache = null;
        this.cacheTimestamp = null;
        this.cacheTimeout = 5000; // 5 seconds

        // Initialize session detection
        this.detectSession();
    }

    /**
     * Robust session detection with multiple fallback strategies
     */
    detectSession() {
        // Use cache if recent
        if (this.detectionCache && this.cacheTimestamp &&
            (Date.now() - this.cacheTimestamp < this.cacheTimeout)) {
            return this.detectionCache;
        }

        const detection = {
            method: 'unknown',
            sessionId: null,
            windowId: null,
            paneId: null,
            fullId: null,
            processInfo: null
        };

        try {
            // Strategy 1: Direct tmux environment with TTY-based resolution
            // CRITICAL: Don't use #I as it returns the ACTIVE window, not Claude's window!
            if (process.env.TMUX) {
                detection.sessionId = this.getTmuxValue('#S');

                // Always try TTY-based resolution first with more thorough detection
                const processDetection = this.detectViaProcessTree();
                if (processDetection.success && processDetection.windowId) {
                    detection.method = 'tmux_env_with_tty';
                    detection.windowId = processDetection.windowId;
                    detection.paneId = processDetection.paneId || this.getTmuxValue('#P');
                    detection.processInfo = processDetection.processInfo;
                } else {
                    // Enhanced fallback: try TMUX_PANE environment first
                    if (process.env.TMUX_PANE) {
                        // Use TMUX_PANE to find our window via pane lookup
                        const paneWindow = this.getWindowFromPane(process.env.TMUX_PANE);
                        if (paneWindow) {
                            detection.method = 'tmux_env_via_pane';
                            detection.windowId = paneWindow;
                            detection.paneId = process.env.TMUX_PANE.replace('%', '');
                        } else {
                            // Last resort: use active window but mark as fallback
                            detection.method = 'tmux_env_fallback';
                            detection.windowId = this.getTmuxValue('#I');
                            detection.paneId = this.getTmuxValue('#P');
                        }
                    } else {
                        // Final fallback
                        detection.method = 'tmux_env_fallback';
                        detection.windowId = this.getTmuxValue('#I');
                        detection.paneId = this.getTmuxValue('#P');
                    }
                }

                detection.fullId = `${detection.sessionId}-${detection.windowId}-${detection.paneId}`;
            }

            // Strategy 2: Environment variable fallback
            else if (process.env.TMUX_PANE) {
                detection.method = 'tmux_pane_env';
                const paneNum = process.env.TMUX_PANE.replace('%', '');
                detection.paneId = paneNum;
                detection.fullId = `claude-${paneNum}`;
            }

            // Strategy 3: Process tree traversal (complex but thorough)
            else {
                const processDetection = this.detectViaProcessTree();
                if (processDetection.success) {
                    detection.method = 'process_tree';
                    Object.assign(detection, processDetection);
                }
            }

            // Strategy 4: Ultimate fallback to process ID
            if (!detection.fullId) {
                detection.method = 'pid_fallback';
                detection.fullId = `claude-${process.ppid}-${process.pid}`;
                detection.processInfo = { ppid: process.ppid, pid: process.pid };
            }

            // Store successful detection
            this.sessionId = detection.sessionId;
            this.windowId = detection.windowId;
            this.paneId = detection.paneId;
            this.processInfo = detection.processInfo;

        } catch (error) {
            // Ultimate failsafe
            detection.method = 'error_fallback';
            detection.fullId = `claude-emergency-${Date.now()}`;
        }

        // Cache the result
        this.detectionCache = detection;
        this.cacheTimestamp = Date.now();

        // Log detection for debugging
        this.logDetection(detection);

        return detection;
    }

    /**
     * Get tmux variable value safely
     */
    getTmuxValue(variable) {
        try {
            return execSync(`tmux display-message -p "${variable}"`, {
                encoding: 'utf8',
                timeout: 2000,
                stdio: 'pipe'
            }).trim();
        } catch (e) {
            return null;
        }
    }

    /**
     * Complex process tree traversal for tmux detection
     */
    detectViaProcessTree() {
        const result = {
            success: false,
            sessionId: null,
            windowId: null,
            paneId: null,
            fullId: null,
            processInfo: null
        };

        try {
            let currentPid = process.ppid;
            let attempts = 0;
            const maxAttempts = 10;
            const processTrace = [];

            while (currentPid && attempts < maxAttempts) {
                // Get TTY for this process
                const tty = this.getProcessTTY(currentPid);
                processTrace.push(`${currentPid}:${tty || 'no-tty'}`);

                if (tty) {
                    // Try to find tmux window with this TTY
                    const tmuxInfo = this.findTmuxByTTY(tty);
                    if (tmuxInfo.success) {
                        result.success = true;
                        result.sessionId = tmuxInfo.sessionId;
                        result.windowId = tmuxInfo.windowId;
                        result.paneId = tmuxInfo.paneId;
                        result.fullId = `${tmuxInfo.sessionId}-${tmuxInfo.windowId}-${tmuxInfo.paneId}`;
                        result.processInfo = {
                            tty,
                            processTrace,
                            finalPid: currentPid
                        };
                        break;
                    }
                }

                // Go up the process tree
                try {
                    const parentPid = execSync(`ps -o ppid= -p ${currentPid} 2>/dev/null`, {
                        encoding: 'utf8',
                        timeout: 1000
                    }).trim();

                    if (!parentPid || parentPid === '1' || parentPid === currentPid) {
                        break;
                    }

                    currentPid = parentPid;
                    attempts++;
                } catch (e) {
                    break;
                }
            }

            result.processInfo = { processTrace };
        } catch (error) {
            result.processInfo = { error: error.message };
        }

        return result;
    }

    /**
     * Get TTY for a process ID
     */
    getProcessTTY(pid) {
        try {
            const tty = execSync(`ps -o tty= -p ${pid} 2>/dev/null`, {
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
     * Get window ID from pane ID using tmux list-panes
     */
    getWindowFromPane(paneId) {
        try {
            const panes = execSync(
                'tmux list-panes -a -F "#{window_index}:#{pane_id}"',
                { encoding: 'utf8', timeout: 3000 }
            ).trim().split('\n');

            for (const pane of panes) {
                const [windowIndex, currentPaneId] = pane.split(':');
                if (currentPaneId === paneId) {
                    return windowIndex;
                }
            }
        } catch (e) {}
        return null;
    }

    /**
     * Find tmux session/window/pane by TTY
     */
    findTmuxByTTY(tty) {
        const result = { success: false };

        try {
            // Get detailed pane information
            const panes = execSync(
                'tmux list-panes -a -F "#{session_name}:#{window_index}:#{pane_index}:#{pane_tty}"',
                { encoding: 'utf8', timeout: 3000 }
            ).trim().split('\n');

            for (const pane of panes) {
                const [sessionName, windowIndex, paneIndex, paneTty] = pane.split(':');
                if (paneTty === tty) {
                    result.success = true;
                    result.sessionId = sessionName;
                    result.windowId = windowIndex;
                    result.paneId = paneIndex;
                    break;
                }
            }
        } catch (e) {}

        return result;
    }

    /**
     * Get the current session identifier
     */
    getSessionId() {
        const detection = this.detectSession();
        return detection.fullId;
    }

    /**
     * Get comprehensive session information including conversation UUID and window ID
     */
    getSessionInfo() {
        const detection = this.detectSession();
        const fs = require('fs');

        // Try to get conversation UUID from the window mapping file
        let conversationId = null;
        let windowId = null;

        try {
            const conversationFile = '/tmp/claude-window-conversations.json';
            if (fs.existsSync(conversationFile)) {
                const conversations = JSON.parse(fs.readFileSync(conversationFile, 'utf8'));

                // Use the same detection logic as context-detector.js
                windowId = this.detectCurrentWindowIdFromConversations(conversations);
                if (windowId && conversations[windowId]) {
                    conversationId = conversations[windowId];
                }
            }
        } catch (e) {
            // Fallback if conversation mapping fails
        }

        return {
            sessionId: detection.fullId,
            conversationId: conversationId,
            windowId: windowId,
            sessionType: detection.sessionType || 'unknown',
            method: detection.method || 'unknown'
        };
    }

    /**
     * Detect current window ID from conversation mapping
     */
    detectCurrentWindowIdFromConversations(conversations) {
        // Simple heuristic: use process ID correlation
        const pid = process.pid;
        const pidStr = pid.toString();

        // Use last digit of PID to correlate with window
        const windowKeys = Object.keys(conversations).map(k => parseInt(k));
        if (windowKeys.length > 0) {
            const lastDigit = parseInt(pidStr.slice(-1));
            const possibleWindow = windowKeys.find(k => k % 10 === lastDigit % 10);
            if (possibleWindow) {
                return possibleWindow.toString();
            }

            // Fallback: use most recent window
            const sortedKeys = windowKeys.sort((a, b) => b - a);
            return sortedKeys[0].toString();
        }

        return null;
    }

    /**
     * Get the current window ID (for tmux operations)
     * CRITICAL: Returns the window where Claude is running, NOT the active window
     */
    getWindowId() {
        const detection = this.detectSession();

        // Log warning if we had to fall back to active window
        if (detection.method === 'tmux_env_fallback' && detection.windowId) {
            this.logDetection({
                ...detection,
                warning: 'Using active window as fallback - may be incorrect!'
            });
        }

        return detection.windowId || null;  // Return null instead of 'current' to avoid confusion
    }

    /**
     * Get session-specific file path for state storage
     */
    getSessionFile(baseFilename, extension = '.json') {
        const sessionId = this.getSessionId();
        const safeSessionId = sessionId.replace(/[^a-zA-Z0-9-_]/g, '_');
        const filename = baseFilename.replace(/\.[^.]*$/, ''); // Remove existing extension
        return `/tmp/claude-${filename}-${safeSessionId}${extension}`;
    }

    /**
     * Load session-specific data with backwards compatibility
     */
    loadSessionData(baseFilename, defaultData = {}) {
        const sessionFile = this.getSessionFile(baseFilename);

        try {
            if (fs.existsSync(sessionFile)) {
                const data = JSON.parse(fs.readFileSync(sessionFile, 'utf8'));
                // Migrate Sets if needed (for QoL manager compatibility)
                if (data.stats && Array.isArray(data.stats.unique_files)) {
                    data.stats.unique_files = new Set(data.stats.unique_files);
                }
                if (data.stats && Array.isArray(data.stats.unique_tools)) {
                    data.stats.unique_tools = new Set(data.stats.unique_tools);
                }
                return data;
            }
        } catch (e) {
            this.logError('loadSessionData', e, { sessionFile });
        }

        // Try to migrate from legacy global file
        const legacyData = this.migrateLegacyData(baseFilename, defaultData);
        if (legacyData) {
            this.saveSessionData(baseFilename, legacyData);
            return legacyData;
        }

        return defaultData;
    }

    /**
     * Save session-specific data
     */
    saveSessionData(baseFilename, data) {
        const sessionFile = this.getSessionFile(baseFilename);

        try {
            // Convert Sets to Arrays for JSON serialization
            const serializable = JSON.parse(JSON.stringify(data, (key, value) => {
                if (value instanceof Set) {
                    return Array.from(value);
                }
                return value;
            }));

            fs.writeFileSync(sessionFile, JSON.stringify(serializable, null, 2));
            return true;
        } catch (e) {
            this.logError('saveSessionData', e, { sessionFile, dataKeys: Object.keys(data) });
            return false;
        }
    }

    /**
     * Migrate data from legacy global files
     */
    migrateLegacyData(baseFilename, defaultData) {
        const legacyPaths = [
            `/tmp/claude-qol-session.json`,
            `/tmp/claude-${baseFilename}.json`,
            path.join(__dirname, `../data/${baseFilename}.json`)
        ];

        for (const legacyPath of legacyPaths) {
            try {
                if (fs.existsSync(legacyPath)) {
                    const data = JSON.parse(fs.readFileSync(legacyPath, 'utf8'));
                    this.logMigration(legacyPath, this.getSessionFile(baseFilename));
                    return data;
                }
            } catch (e) {
                this.logError('migrateLegacyData', e, { legacyPath });
            }
        }

        return null;
    }

    /**
     * Get current work context for this Claude instance
     */
    getWorkContext() {
        if (!this.workContext) {
            this.workContext = this.detectOrLoadWorkContext();
        }
        return this.workContext;
    }

    /**
     * Detect work context based on recent activity
     */
    detectOrLoadWorkContext() {
        const contextFile = '.claude/active-context.json';

        // Try to load existing context first
        try {
            if (fs.existsSync(contextFile)) {
                const data = JSON.parse(fs.readFileSync(contextFile, 'utf8'));
                const age = Date.now() - data.established;

                // If context is less than 2 hours old and confidence is high, use it
                if (age < 2 * 60 * 60 * 1000 && data.confidence > 0.7) {
                    return data.context;
                }
            }
        } catch (e) {
            this.logError('loadWorkContext', e);
        }

        // Auto-detect context from recent activity
        const detectedContext = this.detectWorkContextFromActivity();
        this.lockWorkContext(
            detectedContext.context,
            detectedContext.confidence,
            detectedContext.method || 'auto',
            detectedContext.reasoning || null
        );

        return detectedContext.context;
    }

    /**
     * Analyze recent file activity to determine work context
     */
    detectWorkContextFromActivity() {
        const recentActivity = this.getRecentFileActivity();

        // Try semantic context detection via MCP first
        const semanticResult = this.detectContextViaMCP(recentActivity);
        if (semanticResult && semanticResult.confidence > 0.5) {
            return {
                context: semanticResult.context,
                confidence: semanticResult.confidence,
                scores: semanticResult.scores,
                method: 'semantic_mcp',
                reasoning: semanticResult.reasoning,
                drift_detected: semanticResult.drift_detected
            };
        }

        // Fallback to pattern-based detection
        const patterns = this.CONTEXT_PATTERNS;
        let scores = {};

        // Score each potential context
        for (const [contextName, pattern] of Object.entries(patterns)) {
            scores[contextName] = this.scoreContextMatch(recentActivity, pattern);
        }

        // Find highest scoring context
        const bestMatch = Object.entries(scores)
            .sort(([,a], [,b]) => b - a)[0];

        return {
            context: bestMatch[0],
            confidence: bestMatch[1],
            scores: scores,
            method: 'pattern_fallback'
        };
    }

    /**
     * Detect context using semantic understanding via MCP server (synchronous)
     */
    detectContextViaMCP(recentActivity) {
        try {
            // Skip MCP queries outside tengine project directory
            if (!process.cwd().includes('/tengine')) {
                return null;
            }

            const { execSync } = require('child_process');

            // Prepare operation descriptions from recent activity
            const operationDescriptions = this.createOperationDescriptions(recentActivity);

            const requestData = {
                tool: 'detect_context',
                params: {
                    recent_files: recentActivity.files || [],
                    recent_tools: recentActivity.tools || [],
                    operation_descriptions: operationDescriptions,
                    current_context: this.workContext,
                    confidence_threshold: 0.7,
                    agent_id: process.pid.toString()
                }
            };

            const postData = JSON.stringify(requestData);

            // Use curl for synchronous HTTP request with timeout
            const curlCommand = `curl -s -X POST -H "Content-Type: application/json" ` +
                `--max-time 3 --connect-timeout 1 ` +
                `-d '${postData.replace(/'/g, "'\\''")}' ` +
                `http://localhost:3001/mcp`;

            const result = execSync(curlCommand, {
                encoding: 'utf8',
                timeout: 5000, // 5 second total timeout
                stdio: 'pipe'
            });

            if (!result || result.trim() === '') {
                throw new Error('Empty response from MCP server');
            }

            const response = JSON.parse(result.trim());

            if (response.success && response.result) {
                return response.result;
            } else {
                throw new Error(`MCP error: ${response.error || 'Unknown error'}`);
            }

        } catch (e) {
            // Log the error but don't throw - we'll fall back to pattern matching
            this.logError('detectContextViaMCP', e);
            return null;
        }
    }

    /**
     * Create natural language descriptions of recent operations
     */
    createOperationDescriptions(recentActivity) {
        const descriptions = [];

        if (recentActivity.files && recentActivity.files.length > 0) {
            const fileTypes = this.categorizeFiles(recentActivity.files);

            if (fileTypes.rust.length > 0) {
                descriptions.push(`Working with Rust source files: ${fileTypes.rust.slice(0, 3).join(', ')}`);
            }
            if (fileTypes.shaders.length > 0) {
                descriptions.push(`Editing shader files: ${fileTypes.shaders.slice(0, 3).join(', ')}`);
            }
            if (fileTypes.javascript.length > 0) {
                descriptions.push(`Modifying JavaScript/Node.js files: ${fileTypes.javascript.slice(0, 3).join(', ')}`);
            }
            if (fileTypes.config.length > 0) {
                descriptions.push(`Updating configuration files: ${fileTypes.config.slice(0, 3).join(', ')}`);
            }
        }

        if (recentActivity.tools && recentActivity.tools.length > 0) {
            const toolCategories = this.categorizeTools(recentActivity.tools);

            if (toolCategories.development.length > 0) {
                descriptions.push(`Using development tools: ${toolCategories.development.join(', ')}`);
            }
            if (toolCategories.analysis.length > 0) {
                descriptions.push(`Performing analysis with: ${toolCategories.analysis.join(', ')}`);
            }
            if (toolCategories.build.length > 0) {
                descriptions.push(`Building/testing with: ${toolCategories.build.join(', ')}`);
            }
        }

        return descriptions;
    }

    /**
     * Categorize files by type for better semantic understanding
     */
    categorizeFiles(files) {
        const categories = {
            rust: [],
            shaders: [],
            javascript: [],
            config: [],
            other: []
        };

        for (const file of files) {
            const ext = file.split('.').pop()?.toLowerCase();
            const basename = file.split('/').pop() || file;

            if (ext === 'rs') {
                categories.rust.push(basename);
            } else if (['glsl', 'comp', 'vert', 'frag', 'spv'].includes(ext)) {
                categories.shaders.push(basename);
            } else if (['js', 'json', 'ts'].includes(ext)) {
                categories.javascript.push(basename);
            } else if (['toml', 'yaml', 'yml', 'md', 'txt'].includes(ext)) {
                categories.config.push(basename);
            } else {
                categories.other.push(basename);
            }
        }

        return categories;
    }

    /**
     * Categorize tools by purpose
     */
    categorizeTools(tools) {
        const categories = {
            development: [],
            analysis: [],
            build: [],
            other: []
        };

        const toolMap = {
            development: ['Read', 'Edit', 'Write', 'MultiEdit', 'TodoWrite'],
            analysis: ['search', 'analyze', 'call_chain', 'Grep', 'Glob', 'extract_functions'],
            build: ['build', 'validate_shader', 'debug_analyzer', 'Bash']
        };

        for (const tool of tools) {
            let categorized = false;
            for (const [category, categoryTools] of Object.entries(toolMap)) {
                if (categoryTools.some(t => tool.toLowerCase().includes(t.toLowerCase()))) {
                    categories[category].push(tool);
                    categorized = true;
                    break;
                }
            }
            if (!categorized) {
                categories.other.push(tool);
            }
        }

        return categories;
    }

    /**
     * Context patterns for detecting work focus
     */
    get CONTEXT_PATTERNS() {
        return {
            'dashboard-improvements': {
                files: [
                    'session_dashboard/**',
                    'claude_dash.rs',
                    'productivity-dashboard.js',
                    'achievement*.js'
                ],
                tools: ['Read', 'Edit', 'TodoWrite', 'MultiEdit'],
                keywords: ['session', 'dashboard', 'analytics', 'achievement', 'productivity'],
                anti_patterns: ['shader', 'gpu_context', 'vulkan', 'dgc_hal', 'ext_'],
                weight: 1.0
            },

            'gpu-engine-work': {
                files: [
                    'crates/tengine-dgc-hal/**',
                    'csrc/ext_*.c',
                    'shaders/**/*.comp',
                    'gpu_context.rs',
                    'orchestrator*.comp'
                ],
                tools: ['validate_shader', 'build', 'analyze', 'debug_analyzer'],
                keywords: ['gpu', 'dgc', 'orchestrator', 'pipeline', 'vulkan', 'shader'],
                anti_patterns: ['dashboard', 'achievement', 'session_', 'qol'],
                weight: 1.0
            },

            'shader-debugging': {
                files: [
                    'shaders/**',
                    '*.glsl',
                    '*.comp',
                    '*.vert',
                    '*.frag',
                    'shader_validator.rs'
                ],
                tools: ['validate_shader', 'shader_search', 'debug_analyzer', 'Grep'],
                keywords: ['shader', 'spirv', 'glsl', 'validation', 'compile', 'debug'],
                anti_patterns: ['dashboard', 'hooks', 'achievement', 'qol'],
                weight: 1.0
            },

            'mcp-tools-optimization': {
                files: [
                    'src/mcp_handlers/**',
                    'mcp_*.rs',
                    'tool_*.rs',
                    'search*.rs'
                ],
                tools: ['search', 'analyze', 'call_chain', 'smart_refactor'],
                keywords: ['mcp', 'tool', 'search', 'semantic', 'embeddings'],
                anti_patterns: ['gpu', 'shader', 'dashboard'],
                weight: 0.9
            },

            'general-development': {
                files: ['**'],
                tools: ['Read', 'Edit', 'Write'],
                keywords: [],
                anti_patterns: [],
                weight: 0.3  // Fallback with low weight
            }
        };
    }

    /**
     * Score how well recent activity matches a context pattern
     */
    scoreContextMatch(activity, pattern) {
        let score = 0;
        let totalWeight = 0;

        // Score file patterns
        for (const file of activity.files) {
            for (const filePattern of pattern.files) {
                if (this.matchesPattern(file, filePattern)) {
                    score += 0.4;
                    totalWeight += 0.4;
                    break;
                }
            }
        }

        // Score tool usage
        for (const tool of activity.tools) {
            if (pattern.tools.includes(tool)) {
                score += 0.3;
                totalWeight += 0.3;
            }
        }

        // Score keywords in file paths and content
        const allText = [...activity.files, ...activity.tools].join(' ').toLowerCase();
        for (const keyword of pattern.keywords) {
            if (allText.includes(keyword.toLowerCase())) {
                score += 0.2;
                totalWeight += 0.2;
            }
        }

        // Penalize anti-patterns
        for (const antiPattern of pattern.anti_patterns) {
            if (allText.includes(antiPattern.toLowerCase())) {
                score -= 0.3;
            }
        }

        // Apply pattern weight
        score *= pattern.weight;

        // Normalize score
        return totalWeight > 0 ? Math.max(0, score / totalWeight) : 0;
    }

    /**
     * Check if a file path matches a pattern (supports glob-like patterns)
     */
    matchesPattern(filePath, pattern) {
        // Convert glob pattern to regex
        const regexPattern = pattern
            .replace(/\*\*/g, '.*')
            .replace(/\*/g, '[^/]*')
            .replace(/\./g, '\\.');

        const regex = new RegExp(regexPattern, 'i');
        return regex.test(filePath);
    }

    /**
     * Get recent file activity from various sources
     */
    getRecentFileActivity() {
        const activity = {
            files: [],
            tools: []
        };

        try {
            // Try to get from QoL session if available
            const qolFile = this.getSessionFile('qol-session');
            if (fs.existsSync(qolFile)) {
                const qolData = JSON.parse(fs.readFileSync(qolFile, 'utf8'));
                const recentOps = qolData.operations ? qolData.operations.slice(-10) : [];

                for (const op of recentOps) {
                    if (op.toolName) activity.tools.push(op.toolName);
                    if (op.toolInput && op.toolInput.file_path) {
                        activity.files.push(op.toolInput.file_path);
                    }
                    if (op.toolInput && op.toolInput.pattern) {
                        activity.files.push(op.toolInput.pattern);
                    }
                }
            }

            // Also check recent git activity as a hint
            try {
                const gitFiles = require('child_process')
                    .execSync('git diff --name-only HEAD~5 2>/dev/null || echo ""', { encoding: 'utf8' })
                    .split('\n')
                    .filter(f => f.trim());
                activity.files.push(...gitFiles);
            } catch (e) {
                // Git not available or not in git repo
            }
        } catch (e) {
            this.logError('getRecentFileActivity', e);
        }

        return activity;
    }

    /**
     * Lock in a work context with confidence score
     */
    lockWorkContext(context, confidence, method = 'auto', reasoning = null) {
        try {
            const contextFile = '.claude/active-context.json';
            const contextDir = '.claude';

            // Ensure .claude directory exists
            if (!fs.existsSync(contextDir)) {
                fs.mkdirSync(contextDir, { recursive: true });
            }

            // Track context history for drift detection
            this.trackContextHistory(context, confidence, method);

            // Analyze drift
            const driftAnalysis = this.analyzeDrift();

            const data = {
                context,
                confidence,
                established: Date.now(),
                detection_method: method,
                drift_warnings: driftAnalysis.drift_detected ? 1 : 0,
                claude_pid: process.pid,
                reasoning,
                drift_analysis: driftAnalysis
            };

            fs.writeFileSync(contextFile, JSON.stringify(data, null, 2));
            this.logWorkContextChange(context, confidence, method, driftAnalysis);

        } catch (e) {
            this.logError('lockWorkContext', e);
        }
    }

    /**
     * Track context history for drift analysis
     */
    trackContextHistory(context, confidence, method) {
        try {
            const historyFile = '.claude/context-history.json';
            let history = [];

            // Load existing history
            if (fs.existsSync(historyFile)) {
                try {
                    history = JSON.parse(fs.readFileSync(historyFile, 'utf8'));
                } catch (e) {
                    history = [];
                }
            }

            // Add new entry
            history.push({
                context,
                confidence,
                timestamp: Date.now(),
                method,
                pid: process.pid
            });

            // Keep only last 50 entries
            if (history.length > 50) {
                history = history.slice(-50);
            }

            fs.writeFileSync(historyFile, JSON.stringify(history, null, 2));

        } catch (e) {
            this.logError('trackContextHistory', e);
        }
    }

    /**
     * Analyze context drift using MCP server
     */
    analyzeDrift() {
        try {
            const historyFile = '.claude/context-history.json';
            if (!fs.existsSync(historyFile)) {
                return { drift_detected: false, confidence_trend: 0.0 };
            }

            const history = JSON.parse(fs.readFileSync(historyFile, 'utf8'));
            if (history.length < 3) {
                return { drift_detected: false, confidence_trend: 0.0 };
            }

            // Try MCP-based drift analysis first
            const mcpResult = this.analyzeDriftViaMCP(history);
            if (mcpResult) {
                return mcpResult;
            }

            // Fallback to simple local analysis
            return this.analyzeDriftLocally(history);

        } catch (e) {
            this.logError('analyzeDrift', e);
            return { drift_detected: false, confidence_trend: 0.0 };
        }
    }

    /**
     * Analyze drift using MCP server
     */
    analyzeDriftViaMCP(history) {
        try {
            // Skip MCP queries outside tengine project directory
            if (!process.cwd().includes('/tengine')) {
                return null;
            }

            const { execSync } = require('child_process');

            const requestData = {
                tool: 'analyze_drift',
                params: {
                    context_history: history.slice(-10), // Last 10 entries
                    drift_threshold: 0.3
                }
            };

            const postData = JSON.stringify(requestData);
            const curlCommand = `curl -s -X POST -H "Content-Type: application/json" ` +
                `--max-time 2 --connect-timeout 1 ` +
                `-d '${postData.replace(/'/g, "'\\''")}' ` +
                `http://localhost:3001/mcp`;

            const result = execSync(curlCommand, {
                encoding: 'utf8',
                timeout: 3000,
                stdio: 'pipe'
            });

            if (!result || result.trim() === '') {
                return null;
            }

            const response = JSON.parse(result.trim());
            if (response.success && response.result) {
                return response.result;
            }

            return null;

        } catch (e) {
            this.logError('analyzeDriftViaMCP', e);
            return null;
        }
    }

    /**
     * Simple local drift analysis as fallback
     */
    analyzeDriftLocally(history) {
        const recent = history.slice(-5);
        const confidences = recent.map(h => h.confidence);
        const contexts = recent.map(h => h.context);

        // Calculate confidence trend
        const avgRecent = confidences.slice(-2).reduce((a, b) => a + b, 0) / 2;
        const avgOlder = confidences.slice(0, -2).reduce((a, b) => a + b, 0) / (confidences.length - 2);
        const confidence_trend = avgRecent - avgOlder;

        // Count context switches
        const switches = contexts.slice(1).filter((ctx, i) => ctx !== contexts[i]).length;
        const drift_detected = confidence_trend < -0.3 || switches > 2;

        return {
            drift_detected,
            confidence_trend,
            suggested_action: drift_detected ?
                'Context appears unstable. Consider reviewing work focus.' :
                'Context appears stable.',
            stability_score: 1.0 - (switches / contexts.length),
            trend_analysis: `Confidence trend: ${(confidence_trend * 100).toFixed(1)}%, ${switches} switches`
        };
    }

    /**
     * Log work context changes
     */
    logWorkContextChange(context, confidence, method = 'auto', driftAnalysis = null) {
        try {
            const logEntry = {
                timestamp: new Date().toISOString(),
                type: 'work_context_change',
                context,
                confidence,
                detection_method: method,
                drift_analysis: driftAnalysis,
                pid: process.pid
            };

            fs.appendFileSync('/tmp/session-manager.log',
                JSON.stringify(logEntry) + '\n');
        } catch (e) {
            // Silent fail for logging
        }
    }

    /**
     * Get session context for debugging/logging
     */
    getSessionContext() {
        const detection = this.detectSession();
        return {
            sessionId: this.getSessionId(),
            windowId: this.getWindowId(),
            workContext: this.getWorkContext(),
            detection: detection,
            pid: process.pid,
            ppid: process.ppid,
            timestamp: Date.now()
        };
    }

    /**
     * Log session detection for debugging
     */
    logDetection(detection) {
        try {
            const logEntry = {
                timestamp: new Date().toISOString(),
                type: 'session_detection',
                detection: detection,
                pid: process.pid,
                ppid: process.ppid
            };

            fs.appendFileSync('/tmp/session-manager.log',
                JSON.stringify(logEntry) + '\n');
        } catch (e) {
            // Silent fail for logging
        }
    }

    /**
     * Log errors for debugging
     */
    logError(operation, error, context = {}) {
        try {
            const logEntry = {
                timestamp: new Date().toISOString(),
                type: 'error',
                operation,
                error: error.message,
                context,
                sessionId: this.getSessionId()
            };

            fs.appendFileSync('/tmp/session-manager.log',
                JSON.stringify(logEntry) + '\n');
        } catch (e) {
            // Silent fail for logging
        }
    }

    /**
     * Log migration events
     */
    logMigration(from, to) {
        try {
            const logEntry = {
                timestamp: new Date().toISOString(),
                type: 'migration',
                from,
                to,
                sessionId: this.getSessionId()
            };

            fs.appendFileSync('/tmp/session-manager.log',
                JSON.stringify(logEntry) + '\n');
        } catch (e) {
            // Silent fail for logging
        }
    }

    /**
     * Track context history for drift detection
     */
    trackContextHistory(context, confidence, method = 'unknown') {
        const contextHistoryFile = '.claude/context-history.json';

        try {
            let history = [];
            if (fs.existsSync(contextHistoryFile)) {
                history = JSON.parse(fs.readFileSync(contextHistoryFile, 'utf8'));
            }

            const sessionInfo = this.getSessionInfo();
            const entry = {
                context,
                confidence,
                method,
                timestamp: Date.now(),
                session_id: sessionInfo.sessionId,
                conversation_id: sessionInfo.conversationId,
                window_id: sessionInfo.windowId,
                session_type: sessionInfo.sessionType
            };

            history.unshift(entry);
            // Keep last 100 entries
            if (history.length > 100) {
                history = history.slice(0, 100);
            }

            fs.writeFileSync(contextHistoryFile, JSON.stringify(history, null, 2));

            // Check for drift
            this.analyzeDriftFromHistory(history);

        } catch (e) {
            this.logError('trackContextHistory', e);
        }
    }

    /**
     * Analyze context drift using MCP drift analyzer
     */
    analyzeDriftFromHistory(history) {
        try {
            if (history.length < 3) return; // Need minimum history

            // Skip MCP queries outside tengine project directory
            if (!process.cwd().includes('/tengine')) {
                return;
            }

            const { execSync } = require('child_process');

            const mcpRequest = {
                jsonrpc: "2.0",
                id: "drift_analysis",
                method: "tools/call",
                params: {
                    name: "analyze_drift",
                    arguments: {
                        context_history: history.slice(0, 5) // Reduced to 5 entries to avoid 422 errors
                    }
                }
            };

            const postData = JSON.stringify(mcpRequest);
            const curlCommand = `curl -s -X POST -H "Content-Type: application/json" ` +
                `--max-time 2 --connect-timeout 1 ` +
                `-d '${postData.replace(/'/g, "'\\''")}' ` +
                `http://localhost:3001/mcp`;

            const result = execSync(curlCommand, {
                encoding: 'utf8',
                timeout: 3000,
                stdio: 'pipe'
            });

            const response = JSON.parse(result);
            if (response.result && response.result.drift_data) {
                const driftData = response.result.drift_data;

                if (driftData.drift_detected) {
                    this.logDriftDetection(driftData);
                }
            }

        } catch (error) {
            // Silent fail for drift analysis
        }
    }

    /**
     * Log context drift detection
     */
    logDriftDetection(driftData) {
        try {
            const logEntry = {
                timestamp: new Date().toISOString(),
                type: 'context_drift',
                drift_detected: driftData.drift_detected,
                confidence_trend: driftData.confidence_trend,
                stability_score: driftData.stability_score,
                suggested_action: driftData.suggested_action,
                sessionId: this.getSessionId()
            };

            fs.appendFileSync('/tmp/session-manager.log',
                JSON.stringify(logEntry) + '\n');

            // Also save drift alert for dashboard
            const driftAlertFile = '.claude/drift-alerts.json';
            let alerts = [];
            if (fs.existsSync(driftAlertFile)) {
                alerts = JSON.parse(fs.readFileSync(driftAlertFile, 'utf8'));
            }

            alerts.unshift({
                ...logEntry,
                id: Date.now().toString()
            });

            // Keep last 50 alerts
            if (alerts.length > 50) {
                alerts = alerts.slice(0, 50);
            }

            fs.writeFileSync(driftAlertFile, JSON.stringify(alerts, null, 2));

        } catch (e) {
            // Silent fail for logging
        }
    }

    /**
     * Enhanced context detection with history tracking
     */
    detectWorkContextFromActivityEnhanced() {
        const detectedContext = this.detectWorkContextFromActivity();

        // Track this detection in history
        this.trackContextHistory(
            detectedContext.context,
            detectedContext.confidence,
            detectedContext.method
        );

        return detectedContext;
    }
}

// Singleton instance
let sessionManagerInstance = null;

/**
 * Get the singleton session manager instance
 */
function getSessionManager() {
    if (!sessionManagerInstance) {
        sessionManagerInstance = new SessionManager();
    }
    return sessionManagerInstance;
}

/**
 * Convenience functions for common operations
 */
function getSessionId() {
    return getSessionManager().getSessionId();
}

function getWindowId() {
    return getSessionManager().getWindowId();
}

function loadSessionData(baseFilename, defaultData = {}) {
    return getSessionManager().loadSessionData(baseFilename, defaultData);
}

function saveSessionData(baseFilename, data) {
    return getSessionManager().saveSessionData(baseFilename, data);
}

function getWorkContext() {
    return getSessionManager().getWorkContext();
}

function getSessionContext() {
    return getSessionManager().getSessionContext();
}

module.exports = {
    SessionManager,
    getSessionManager,
    getSessionId,
    getWindowId,
    getWorkContext,
    loadSessionData,
    saveSessionData,
    getSessionContext
};