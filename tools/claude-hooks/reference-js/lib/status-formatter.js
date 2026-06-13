/**
 * Centralized Status Formatter for Claude Code Hooks
 *
 * Provides standardized, configurable status formatting across all hooks.
 * Replaces scattered emoji logic with controlled visual treatment.
 * Now includes enhanced contextual title generation without AI costs.
 */

const { titleIntelligence } = require('./title-intelligence');

// Global configuration for visual intensity - REDUCED for clarity
const VISUAL_CONFIG = {
    intensity: 'minimal', // 'none', 'minimal', 'moderate', 'high'
    maxLength: 16,        // REDUCED: Maximum character limit for status text
    minLength: 8,         // REDUCED: Minimum width (no excessive padding)
    enableColors: false,  // Terminal color codes (disabled by default)
    enableEmojis: false   // Emoji usage (disabled by default for clean output)
};

/**
 * Main status formatting function with consistent "Status: xxx" pattern
 * @param {string} text - The status text (will be truncated if too long)
 * @param {string} intent - Visual intent: 'working', 'success', 'error', 'info', 'minimal'
 * @param {object} options - Override options { intensity, maxLength, context, usePrefix }
 * @returns {string} Formatted status string
 */
function formatStatus(text, intent = 'info', options = {}) {
    const config = { ...VISUAL_CONFIG, ...options };

    // Use consistent "Status: " prefix format by default
    if (config.usePrefix !== false) {
        return formatStatusWithPrefix(text, intent, config);
    }

    // Enforce character limit
    const truncatedText = truncateText(text, config.maxLength);

    // Apply intent-based formatting
    switch (intent) {
        case 'working':
            return formatWorking(truncatedText, config);
        case 'success':
            return formatSuccess(truncatedText, config);
        case 'error':
            return formatError(truncatedText, config);
        case 'info':
            return formatInfo(truncatedText, config);
        case 'minimal':
        default:
            return formatMinimal(truncatedText, config);
    }
}

/**
 * Format status with consistent "emoji text" pattern (no "Status:" prefix)
 */
function formatStatusWithPrefix(text, intent = 'info', config = {}) {
    const emoji = getIntentEmoji(intent);

    // Just emoji (2 chars) + space (1 char) = 3 chars overhead
    const availableLength = (config.maxLength || VISUAL_CONFIG.maxLength) - 3;
    const truncatedText = truncateText(text, Math.max(availableLength, 5));

    // Minimal padding
    const minLength = config.minLength || VISUAL_CONFIG.minLength;
    const paddedText = padText(truncatedText, Math.max(minLength - 3, 5));

    return `${emoji} ${truncatedText}`;  // Removed "Status:" prefix
}

/**
 * Get appropriate emoji for each intent type - SIMPLIFIED
 */
function getIntentEmoji(intent) {
    switch (intent) {
        case 'working':
            return '⚡';
        case 'success':
            return '✓';  // Simple checkmark
        case 'error':
            return '✗';  // Simple X
        case 'info':
            return '•';  // Simple bullet
        case 'minimal':
        default:
            return '→';  // Simple arrow
    }
}

/**
 * Truncate text to specified length with ellipsis
 */
function truncateText(text, maxLength) {
    if (!text) return '';
    if (text.length <= maxLength) return text;
    return text.substring(0, maxLength - 3) + '...';
}

/**
 * Pad text to minimum width to prevent rapid visual changes
 */
function padText(text, minLength) {
    if (!text) return ' '.repeat(minLength);
    if (text.length >= minLength) return text;
    return text + ' '.repeat(minLength - text.length);
}

/**
 * Format working/in-progress status
 */
function formatWorking(text, config) {
    switch (config.intensity) {
        case 'high':
            return config.enableEmojis ? `⚡ ${text}...` : `▶ ${text}`;
        case 'moderate':
            return `▶ ${text}`;
        case 'minimal':
            return `${text}...`;
        case 'none':
        default:
            return text;
    }
}

/**
 * Format success status
 */
function formatSuccess(text, config) {
    // Only show success indicators for important operations
    const isImportant = text.includes('build') || text.includes('test') || text.includes('deploy');

    if (!isImportant) {
        return text; // No visual noise for routine operations
    }

    switch (config.intensity) {
        case 'high':
            return config.enableEmojis ? `✅ ${text}` : `✓ ${text}`;
        case 'moderate':
        case 'minimal':
            return `✓ ${text}`;
        case 'none':
        default:
            return text;
    }
}

/**
 * Format error status
 */
function formatError(text, config) {
    switch (config.intensity) {
        case 'high':
            return config.enableEmojis ? `❌ ${text}` : `✗ ${text}`;
        case 'moderate':
        case 'minimal':
            return `✗ ${text}`;
        case 'none':
        default:
            return `${text} (failed)`;
    }
}

/**
 * Format informational status
 */
function formatInfo(text, config) {
    switch (config.intensity) {
        case 'high':
            return config.enableEmojis ? `ℹ️ ${text}` : `• ${text}`;
        case 'moderate':
            return `• ${text}`;
        case 'minimal':
        case 'none':
        default:
            return text;
    }
}

/**
 * Format minimal status (clean text only)
 */
function formatMinimal(text, config) {
    return text;
}

/**
 * Update global visual configuration
 */
function setVisualConfig(newConfig) {
    Object.assign(VISUAL_CONFIG, newConfig);
}

/**
 * Get current visual configuration
 */
function getVisualConfig() {
    return { ...VISUAL_CONFIG };
}

/**
 * Enhanced contextual status formatting with title intelligence
 */
function formatContextualStatus(toolName, toolInput, isSuccess, workflowContext = {}, errorMessage = null) {
    // Use title intelligence to generate contextually relevant title
    const contextualTitle = titleIntelligence.generateContextualTitle(
        toolName,
        toolInput,
        {
            ...workflowContext,
            isSuccess,
            errorType: errorMessage ? extractErrorSummary(errorMessage) : null
        }
    );

    // Return the contextual title directly (it already includes formatting)
    return contextualTitle;
}

/**
 * Utility function for tool-specific status formatting (legacy)
 */
function formatToolStatus(toolName, toolInput, isSuccess, errorMessage = null) {
    // Extract relevant information
    let text = '';
    let intent = isSuccess ? 'success' : 'error';

    if (!isSuccess && errorMessage) {
        // Brief error description
        text = extractErrorSummary(errorMessage);
        intent = 'error';
    } else {
        // Generate clean status based on tool
        text = generateToolStatus(toolName, toolInput);
        intent = isSuccess ? 'success' : 'info';
    }

    return formatStatus(text, intent);
}

/**
 * Extract concise error summary from error message
 */
function extractErrorSummary(errorMessage) {
    const error = errorMessage.toLowerCase();

    // Common error patterns
    if (error.includes('not found')) return 'not found';
    if (error.includes('permission denied')) return 'permission denied';
    if (error.includes('timeout')) return 'timeout';
    if (error.includes('compilation failed') || error.includes('build failed')) return 'build failed';
    if (error.includes('test') && error.includes('failed')) return 'tests failed';
    if (error.includes('network')) return 'network error';

    // Fallback: first few words
    const words = errorMessage.split(' ').slice(0, 3).join(' ');
    return truncateText(words, 20);
}

/**
 * Generate VERY BRIEF status text for different tools
 */
function generateToolStatus(toolName, toolInput) {
    switch (toolName) {
        case 'Bash':
            const cmd = toolInput.command || 'cmd';
            const cmdName = cmd.split(' ')[0].split('/').pop();  // Get just command name
            return cmdName.length > 8 ? cmdName.slice(0, 7) : cmdName;

        case 'Read':
            return 'read';

        case 'Write':
            return 'write';

        case 'Edit':
        case 'MultiEdit':
            return 'edit';

        case 'Grep':
            return 'grep';

        case 'Glob':
            return 'glob';

        case 'WebSearch':
            return 'search';

        case 'WebFetch':
            return 'fetch';

        case 'TodoWrite':
            return 'plan';

        case 'Task':
            return 'agent';

        default:
            // Handle MCP tools
            if (toolName.startsWith('mcp__')) {
                const parts = toolName.split('__');
                const mcpFunction = parts[2] || 'mcp';
                // Shorten common MCP functions
                if (mcpFunction === 'validate_shader') return 'shader';
                if (mcpFunction === 'build') return 'build';
                if (mcpFunction === 'search') return 'search';
                if (mcpFunction === 'debug_analyzer') return 'debug';
                return mcpFunction.slice(0, 8);  // Max 8 chars
            }
            return toolName.slice(0, 8);  // Max 8 chars
    }
}

/**
 * Extract filename from path
 */
function getFileName(filePath) {
    if (!filePath) return 'file';
    const parts = filePath.split('/');
    const fileName = parts[parts.length - 1] || 'file';
    return truncateText(fileName, 15);
}

/**
 * Gather workflow context from available sources
 */
function gatherWorkflowContext() {
    const context = {};

    try {
        // Try to get context from context-detector
        const { contextDetector } = require('./context-detector');
        if (contextDetector && contextDetector.contextData) {
            context.focusArea = contextDetector.contextData.currentContext?.name;
            context.recentFiles = contextDetector.contextData.operations
                ?.slice(0, 5)
                ?.map(op => op.filename)
                ?.filter(Boolean) || [];
        }
    } catch (e) {
        // Silent fail - context detector might not be available
    }

    try {
        // Try to get workflow phase from evolution tracker or session data
        const fs = require('fs');
        const workflowFile = '/tmp/workflow-patterns.json';
        if (fs.existsSync(workflowFile)) {
            const workflowData = JSON.parse(fs.readFileSync(workflowFile, 'utf8'));
            context.workflowPhase = workflowData.workflow_context?.phase || 'focused';
        }
    } catch (e) {
        // Silent fail - use default
        context.workflowPhase = 'focused';
    }

    return context;
}

module.exports = {
    formatStatus,
    formatStatusWithPrefix,
    formatToolStatus,
    formatContextualStatus,
    gatherWorkflowContext,
    setVisualConfig,
    getVisualConfig,
    getIntentEmoji,
    truncateText
};