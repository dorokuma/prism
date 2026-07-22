package main

import "github.com/dorokuma/prism/internal/mcp"

// loadMCPTools reads mcp_tools.json at startup and populates
// the runtime MCP cache so tool_search responses include real definitions
// even before the first namespace bundle arrives from Codex.
var loadMCPTools = mcp.LoadMCPTools
