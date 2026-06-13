#!/usr/bin/env node

/**
 * Smart Context Detection Engine
 * Analyzes patterns of file operations to understand development focus areas
 * and provide intelligent contextual status updates
 */

const fs = require('fs');
const path = require('path');
const { getFileTypeIcon } = require('./file-type-detector');

const CONTEXT_DATA_BASE = '/tmp/claude-context';
const CONTEXT_HISTORY_LIMIT = 50; // Keep last 50 operations for pattern analysis

// Context categories with their patterns and icons
const CONTEXT_CATEGORIES = {
    'hook_dev': {
        icon: '🎣',
        name: 'Hook Development',
        patterns: ['hook', 'tmux', 'claude'],
        paths: ['.claude/hooks'],
        priority: 8
    },
    'shader_dev': {
        icon: '🎨',
        name: 'Shader Programming',
        patterns: ['shader', 'glsl', 'comp', 'vert', 'frag'],
        extensions: ['glsl', 'comp', 'vert', 'frag', 'hlsl'],
        priority: 9
    },
    'rust_dev': {
        icon: '🦀',
        name: 'Rust Development',
        extensions: ['rs', 'toml'],
        paths: ['src', 'crates'],
        priority: 7
    },
    'c_dev': {
        icon: '⚙️',
        name: 'C/C++ Development',
        extensions: ['c', 'h', 'cpp', 'hpp'],
        paths: ['csrc', 'include'],
        priority: 7
    },
    'config_work': {
        icon: '🔧',
        name: 'Configuration',
        extensions: ['json', 'yaml', 'yml', 'toml', 'cfg'],
        patterns: ['config', 'setting', '.env'],
        priority: 5
    },
    'documentation': {
        icon: '📖',
        name: 'Documentation',
        extensions: ['md', 'txt', 'rst'],
        patterns: ['readme', 'doc', 'guide'],
        priority: 4
    },
    'testing': {
        icon: '🧪',
        name: 'Testing',
        patterns: ['test', 'spec', 'mock', 'debug'],
        paths: ['tests', '__tests__'],
        priority: 6
    },
    'build_system': {
        icon: '🔨',
        name: 'Build System',
        patterns: ['cargo', 'build', 'make', 'cmake'],
        extensions: ['lock'],
        priority: 5
    },
    'gpu_architecture': {
        icon: '🌋',
        name: 'GPU Architecture',
        patterns: ['vulkan', 'dgc', 'pipeline', 'buffer', 'render'],
        paths: ['tengine-dgc-hal'],
        priority: 8
    }
};

// Work intensity levels
const INTENSITY_LEVELS = {
    'low': { icon: '🌱', threshold: 2 },
    'medium': { icon: '🔥', threshold: 5 },
    'high': { icon: '⚡', threshold: 10 },
    'intense': { icon: '🚀', threshold: 20 }
};

class ContextDetector {
    constructor() {
        // First generate session ID without relying on contextData
        this.sessionId = this.generateInitialSessionId();
        this.contextData = this.loadContextData();
        // Then update session ID based on loaded context data
        this.sessionId = this.generateSessionId();
        // Clean up old session files periodically
        this.cleanupOldSessionFiles();
    }

    getContextFilePath() {
        return `${CONTEXT_DATA_BASE}-${this.sessionId}.json`;
    }

    loadContextData() {
        const contextFile = this.getContextFilePath();
        try {
            if (fs.existsSync(contextFile)) {
                const data = JSON.parse(fs.readFileSync(contextFile, 'utf8'));
                // Convert filesModified back to Set if it exists as Array
                if (data.productivity && Array.isArray(data.productivity.filesModified)) {
                    data.productivity.filesModified = new Set(data.productivity.filesModified);
                }
                return data;
            }
        } catch (e) {}

        return {
            operations: [],
            sessionStart: Date.now(),
            focusAreas: {},
            currentStreak: null,
            productivity: {
                filesModified: new Set(),
                operationsPerHour: 0,
                focusTime: 0
            }
        };
    }

    saveContextData() {
        try {
            // Convert Set to Array for JSON serialization
            const dataToSave = {
                ...this.contextData,
                productivity: {
                    ...this.contextData.productivity,
                    filesModified: Array.from(this.contextData.productivity.filesModified)
                }
            };
            fs.writeFileSync(this.getContextFilePath(), JSON.stringify(dataToSave));
        } catch (e) {}
    }

    recordOperation(toolName, toolInput, operation = 'edit') {
        const timestamp = Date.now();
        const filename = toolInput.file_path ? path.basename(toolInput.file_path) : null;

        // Use the session identifier established in constructor
        const sessionType = this.detectSessionType();

        const op = {
            timestamp,
            toolName,
            filename,
            fullPath: toolInput.file_path,
            operation,
            command: toolInput.command,
            pattern: toolInput.pattern,
            sessionId: this.sessionId,
            sessionType,
            processId: process.pid,
            parentProcessId: process.ppid
        };

        // Add to operations history
        this.contextData.operations.unshift(op);

        // Keep only recent operations
        if (this.contextData.operations.length > CONTEXT_HISTORY_LIMIT) {
            this.contextData.operations = this.contextData.operations.slice(0, CONTEXT_HISTORY_LIMIT);
        }

        // Update productivity metrics
        if (filename && this.contextData?.productivity?.filesModified?.add) {
            this.contextData.productivity.filesModified.add(filename);
        }

        this.updateFocusAreas();
        this.detectCurrentContext();
        this.saveContextData();
    }

    updateFocusAreas() {
        // Reset focus areas
        this.contextData.focusAreas = {};

        // Analyze recent operations (last 20)
        const recentOps = this.contextData.operations.slice(0, 20);

        for (const op of recentOps) {
            for (const [contextId, context] of Object.entries(CONTEXT_CATEGORIES)) {
                let score = 0;

                // Check file extensions
                if (context.extensions && op.filename) {
                    const ext = op.filename.split('.').pop();
                    if (context.extensions.includes(ext)) {
                        score += 3;
                    }
                }

                // Check path patterns
                if (context.paths && op.fullPath) {
                    for (const pathPattern of context.paths) {
                        if (op.fullPath.includes(pathPattern)) {
                            score += 2;
                        }
                    }
                }

                // Check filename patterns
                if (context.patterns && (op.filename || op.command)) {
                    const searchText = `${op.filename || ''} ${op.command || ''}`.toLowerCase();
                    for (const pattern of context.patterns) {
                        if (searchText.includes(pattern)) {
                            score += 1;
                        }
                    }
                }

                if (score > 0) {
                    this.contextData.focusAreas[contextId] =
                        (this.contextData.focusAreas[contextId] || 0) + score;
                }
            }
        }
    }

    detectCurrentContext() {
        // Try semantic MCP detection first, fall back to pattern matching
        const semanticContext = this.detectContextViaMCP();
        if (semanticContext && semanticContext.confidence > 0.3) {
            this.contextData.currentContext = {
                id: semanticContext.context,
                name: semanticContext.context.replace(/-/g, ' ').replace(/\b\w/g, l => l.toUpperCase()),
                icon: this.getContextIcon(semanticContext.context),
                confidence: semanticContext.confidence,
                intensity: this.calculateIntensity(),
                method: 'semantic_mcp',
                reasoning: semanticContext.reasoning,
                drift_detected: semanticContext.drift_detected,
                scores: semanticContext.scores
            };
            return;
        }

        // Fallback to legacy pattern matching
        if (Object.keys(this.contextData.focusAreas).length === 0) {
            this.contextData.currentContext = null;
            return;
        }

        // Find the dominant focus area
        const sortedAreas = Object.entries(this.contextData.focusAreas)
            .sort(([,a], [,b]) => b - a)
            .filter(([,score]) => score >= 2); // Minimum threshold

        if (sortedAreas.length === 0) {
            this.contextData.currentContext = null;
            return;
        }

        const [dominantContext, score] = sortedAreas[0];
        const context = CONTEXT_CATEGORIES[dominantContext];

        this.contextData.currentContext = {
            id: dominantContext,
            name: context.name,
            icon: context.icon,
            confidence: Math.min(score / 10, 1.0), // Normalize to 0-1
            intensity: this.calculateIntensity(),
            method: 'pattern_matching'
        };
    }

    detectContextViaMCP() {
        // MCP context detection disabled - use pattern matching instead
        return null;
    }

    gatherRecentActivity() {
        const recentOps = this.contextData.operations.slice(0, 15);
        const files = [];
        const tools = [];
        const descriptions = [];

        for (const op of recentOps) {
            if (op.fullPath && !files.includes(op.fullPath)) {
                files.push(op.fullPath);
            }
            if (op.toolName && !tools.includes(op.toolName)) {
                tools.push(op.toolName);
            }
            if (op.operation) {
                descriptions.push(`${op.operation} with ${op.toolName}`);
            }
        }

        return {
            files: files.slice(0, 10), // Limit for API efficiency
            tools: tools.slice(0, 8),
            descriptions: descriptions.slice(0, 5)
        };
    }

    getContextIcon(contextId) {
        const iconMap = {
            'dashboard-improvements': '📊',
            'mcp-tools-optimization': '🔧',
            'shader-debugging': '🎨',
            'gpu-engine-work': '⚡',
            'general-development': '💻'
        };
        return iconMap[contextId] || '📝';
    }

    calculateIntensity() {
        const recentOps = this.contextData.operations.slice(0, 10);
        const timeSpan = 300000; // 5 minutes in milliseconds
        const now = Date.now();

        const recentCount = recentOps.filter(op =>
            now - op.timestamp < timeSpan
        ).length;

        for (const [level, config] of Object.entries(INTENSITY_LEVELS)) {
            if (recentCount >= config.threshold) {
                return { level, ...config };
            }
        }

        return INTENSITY_LEVELS.low;
    }

    getContextualStatus(toolName, toolInput, baseStatus) {
        const context = this.contextData.currentContext;

        if (!context || context.confidence < 0.3) {
            return baseStatus; // Not enough confidence, return basic status
        }

        // Detect operation patterns
        const patterns = this.detectOperationPatterns();
        let enhancement = '';

        if (patterns.iteration) {
            enhancement = ' 🔄';
        } else if (patterns.focus) {
            enhancement = ` ${context.intensity.icon}`;
        }

        return `${context.icon} ${baseStatus}${enhancement}`;
    }

    detectOperationPatterns() {
        const recent = this.contextData.operations.slice(0, 5);
        const patterns = {};

        // Detect iteration pattern (read-edit-read)
        if (recent.length >= 3) {
            const files = recent.map(op => op.filename).filter(Boolean);
            const operations = recent.map(op => op.operation);

            // Same file, alternating read/edit operations
            if (files.length >= 2 &&
                files[0] === files[1] &&
                operations.includes('read') &&
                operations.includes('edit')) {
                patterns.iteration = true;
            }
        }

        // Detect focused work (multiple operations in short time)
        const fiveMinAgo = Date.now() - 300000;
        const recentActive = this.contextData.operations
            .filter(op => op.timestamp > fiveMinAgo).length;

        patterns.focus = recentActive >= 5;

        return patterns;
    }

    findStableParentProcess() {
        const fs = require('fs');
        let currentPid = process.pid;
        let stableParent = process.ppid || 'unknown';

        // Traverse up the process tree to find a stable parent
        for (let i = 0; i < 10; i++) { // Limit depth to prevent infinite loops
            try {
                const statPath = `/proc/${currentPid}/stat`;
                if (!fs.existsSync(statPath)) break;

                const stat = fs.readFileSync(statPath, 'utf8');
                const fields = stat.split(' ');
                if (fields.length < 4) break;

                const parentPid = fields[3];
                const cmdline = this.getProcessCommandLine(parentPid);

                // Stop if we find a claude process or important shell
                if (cmdline.includes('claude') ||
                    cmdline.includes('bash') && !cmdline.includes('timeout') ||
                    cmdline.includes('zsh') ||
                    cmdline.includes('tmux') ||
                    parentPid === '1') {
                    stableParent = parentPid;
                    break;
                }

                currentPid = parentPid;
            } catch (e) {
                break;
            }
        }

        return stableParent;
    }

    getProcessCommandLine(pid) {
        const fs = require('fs');
        try {
            return fs.readFileSync(`/proc/${pid}/cmdline`, 'utf8').replace(/\0/g, ' ');
        } catch (e) {
            return '';
        }
    }

    detectEntrypoint() {
        // Try to detect if this is Codex vs CLI based on environment and process tree

        // Check for explicit environment variable first
        if (process.env.CLAUDE_CODE_ENTRYPOINT) {
            return process.env.CLAUDE_CODE_ENTRYPOINT;
        }

        // Check process tree for claude-codex or GUI indicators
        const processTree = this.getProcessTree();
        for (const cmd of processTree) {
            if (cmd.includes('codex') || cmd.includes('electron') || cmd.includes('vscode')) {
                return 'codex';
            }
        }

        // Check for GUI environment
        if (process.env.DISPLAY &&
            process.env.DISPLAY !== ':99' &&
            !process.env.SSH_CLIENT) {
            // Only consider GUI if not in SSH or test environment
            return 'codex';
        }

        // Check for clear CLI indicators
        if (process.env.SSH_CLIENT ||
            process.env.SSH_TTY ||
            process.env.TMUX ||
            process.env.TERM_PROGRAM ||
            process.env.TERM === 'screen') {
            return 'cli';
        }

        // Check terminal characteristics
        if (process.stdout.isTTY && !process.env.DISPLAY) {
            return 'cli';
        }

        // Default to CLI if uncertain
        return 'cli';
    }

    getProcessTree() {
        const fs = require('fs');
        const commands = [];
        let currentPid = process.pid;

        for (let i = 0; i < 15; i++) {
            try {
                const cmdline = this.getProcessCommandLine(currentPid);
                if (cmdline) {
                    commands.push(cmdline);
                }

                const statPath = `/proc/${currentPid}/stat`;
                if (!fs.existsSync(statPath)) break;

                const stat = fs.readFileSync(statPath, 'utf8');
                const fields = stat.split(' ');
                if (fields.length < 4) break;

                const parentPid = fields[3];
                if (parentPid === '1' || parentPid === currentPid) break;

                currentPid = parentPid;
            } catch (e) {
                break;
            }
        }

        return commands;
    }

    generateInitialSessionId() {
        // Create initial session ID without relying on contextData
        const crypto = require('crypto');
        const envKey = process.env.CLAUDE_CODE_ENTRYPOINT || this.detectEntrypoint();
        const stableParent = this.findStableParentProcess();

        const baseKey = `${envKey}_${stableParent}`;
        const hash = crypto.createHash('sha256').update(baseKey).digest('hex').substring(0, 8);
        return `${envKey}_${stableParent}_${hash}`;
    }

    detectCurrentWindowId(conversations) {
        // Try multiple methods to detect which window/session we're running in

        // Method 1: Check for environment variable set by specific window
        if (process.env.CLAUDE_WINDOW_ID) {
            return process.env.CLAUDE_WINDOW_ID;
        }

        // Method 2: Check for window-specific process artifacts
        const fs = require('fs');

        // Look for window-specific temporary files or working directories
        try {
            // Check if there's a current working directory hint
            const cwd = process.cwd();

            // Method 3: Use process tree analysis to differentiate windows
            const processTree = this.getProcessTree();

            // Look for distinctive patterns in process arguments that might indicate window
            for (const cmd of processTree) {
                // Look for claude commands with window-specific patterns
                if (cmd.includes('--window') || cmd.includes('--session')) {
                    const windowMatch = cmd.match(/--window[=\s]+(\d+)/);
                    if (windowMatch) {
                        return windowMatch[1];
                    }
                }
            }

            // Method 4: Check for existing session files that might indicate window
            const tmpFiles = fs.readdirSync('/tmp').filter(f => f.startsWith('claude-context-'));

            // If we have context files, try to correlate with conversation IDs
            for (const file of tmpFiles) {
                try {
                    const contextPath = `/tmp/${file}`;
                    const contextData = JSON.parse(fs.readFileSync(contextPath, 'utf8'));

                    // Check if this context file has conversation data
                    if (contextData.conversationId) {
                        for (const [winId, convId] of Object.entries(conversations)) {
                            if (convId === contextData.conversationId) {
                                return winId;
                            }
                        }
                    }
                } catch (e) {
                    // Skip malformed context files
                }
            }

        } catch (e) {
            // Ignore errors in window detection
        }

        // Method 5: Use process ID correlation (fallback)
        // This is a heuristic - different windows might have different PID ranges
        const pid = process.pid;
        const pidStr = pid.toString();

        // Simple heuristic: use last digit of PID to correlate with window
        const windowKeys = Object.keys(conversations).map(k => parseInt(k));
        if (windowKeys.length > 0) {
            const lastDigit = parseInt(pidStr.slice(-1));
            const possibleWindow = windowKeys.find(k => k % 10 === lastDigit % 10);
            if (possibleWindow) {
                return possibleWindow.toString();
            }
        }

        return null; // Unable to detect
    }

    generateSessionId() {
        // Create a stable session ID based on conversation UUID and window ID
        const crypto = require('crypto');
        const fs = require('fs');
        const envKey = process.env.CLAUDE_CODE_ENTRYPOINT || this.detectEntrypoint();

        // Try to get conversation UUID from the window mapping file
        let conversationId = null;
        let windowId = null;

        try {
            const conversationFile = '/tmp/claude-window-conversations.json';
            if (fs.existsSync(conversationFile)) {
                const conversations = JSON.parse(fs.readFileSync(conversationFile, 'utf8'));

                // Try to detect current window ID through multiple methods
                windowId = this.detectCurrentWindowId(conversations);
                if (windowId && conversations[windowId]) {
                    conversationId = conversations[windowId];
                } else {
                    // Fallback: use most recent window
                    const windowKeys = Object.keys(conversations).map(k => parseInt(k)).sort((a, b) => b - a);
                    if (windowKeys.length > 0) {
                        windowId = windowKeys[0].toString();
                        conversationId = conversations[windowId];
                    }
                }
            }
        } catch (e) {
            // Fallback if conversation mapping fails
        }

        // Use conversation UUID + window ID if available, otherwise fallback to process-based
        let sessionIdentifier;
        if (conversationId && windowId) {
            sessionIdentifier = `${envKey}_win${windowId}_${conversationId.substring(0, 8)}`;
        } else {
            // Fallback to process-based approach
            const stableParent = this.findStableParentProcess();
            const baseKey = `${envKey}_${stableParent}`;
            const hash = crypto.createHash('sha256').update(baseKey).digest('hex').substring(0, 8);
            sessionIdentifier = `${envKey}_${stableParent}_${hash}`;
        }

        // Store in context for consistency and future correlation
        const baseKey = conversationId ? `${envKey}_${windowId}_${conversationId}` : `${envKey}_${this.findStableParentProcess()}`;
        if (!this.contextData.sessionId || !this.contextData.sessionBaseKey || this.contextData.sessionBaseKey !== baseKey) {
            this.contextData.sessionId = sessionIdentifier;
            this.contextData.sessionBaseKey = baseKey;
            this.contextData.conversationId = conversationId; // Store for future correlation
            this.contextData.windowId = windowId;
        }

        return this.contextData.sessionId;
    }

    detectSessionType() {
        // Detect session type from environment and process context
        const entrypoint = process.env.CLAUDE_CODE_ENTRYPOINT || this.detectEntrypoint();

        if (entrypoint === 'cli') {
            return 'claude-cli';
        } else if (entrypoint === 'codex') {
            return 'claude-codex';
        } else if (process.env.CLAUDECODE) {
            // Generic Claude Code session
            return 'claude-code';
        } else {
            return 'claude-unknown';
        }
    }

    cleanupOldSessionFiles() {
        try {
            const fs = require('fs');
            const path = require('path');

            // Only run cleanup 10% of the time to avoid performance impact
            if (Math.random() > 0.1) return;

            const tmpDir = '/tmp';
            const contextPrefix = 'claude-context-';
            const maxAgeHours = 24; // Keep files for 24 hours
            const maxFiles = 50; // Keep at most 50 files
            const cutoffTime = Date.now() - (maxAgeHours * 60 * 60 * 1000);

            // Get all context files
            const files = fs.readdirSync(tmpDir)
                .filter(file => file.startsWith(contextPrefix) && file.endsWith('.json'))
                .map(file => {
                    const fullPath = path.join(tmpDir, file);
                    const stats = fs.statSync(fullPath);
                    return {
                        name: file,
                        path: fullPath,
                        mtime: stats.mtime.getTime(),
                        size: stats.size
                    };
                })
                .sort((a, b) => b.mtime - a.mtime); // Sort by modification time, newest first

            // Remove old files
            const filesToRemove = files.filter(file => file.mtime < cutoffTime);

            // Remove excess files (keep only the newest maxFiles)
            if (files.length > maxFiles) {
                filesToRemove.push(...files.slice(maxFiles));
            }

            // Actually remove the files
            for (const file of filesToRemove) {
                try {
                    fs.unlinkSync(file.path);
                } catch (e) {
                    // Ignore errors when removing files
                }
            }
        } catch (e) {
            // Silently ignore cleanup errors
        }
    }

    getSessionSummary() {
        const sessionTime = Date.now() - this.contextData.sessionStart;
        const hoursActive = sessionTime / (1000 * 60 * 60);

        return {
            duration: sessionTime,
            filesModified: Array.from(this.contextData.productivity.filesModified).length,
            operationsPerHour: Math.round(this.contextData.operations.length / Math.max(hoursActive, 0.1)),
            currentContext: this.contextData.currentContext,
            focusAreas: this.contextData.focusAreas
        };
    }
}

// Singleton instance
const contextDetector = new ContextDetector();

module.exports = {
    ContextDetector,
    contextDetector,
    CONTEXT_CATEGORIES,
    INTENSITY_LEVELS
};