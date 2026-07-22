package main

import "github.com/dorokuma/prism/internal/mcp"

// sanitizeToolsForChatCompletions converts Responses API tools to Chat Completions format.
var sanitizeToolsForChatCompletions = mcp.SanitizeToolsForChatCompletions

// NamespaceForTool returns the namespace for a prefixed tool name via string parsing.
var NamespaceForTool = mcp.NamespaceForTool

// ResolveNamespaceTool returns the original tool name from a prefixed name.
var ResolveNamespaceTool = mcp.ResolveNamespaceTool

// getSearchToolCache returns a snapshot of cached MCP tools for tool_search interception.
var getSearchToolCache = mcp.GetSearchToolCache

// clearMCPCache clears all cached MCP tools for all tenants.
var clearMCPCache = mcp.ClearMCPCache

// mcpCacheCtxCancel stops the background cache eviction goroutine.
var mcpCacheCtxCancel = mcp.StopMCPCache
