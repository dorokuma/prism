#!/usr/bin/env python3
"""Generate mcp_tools.json from ~/.codex/config.toml MCP server definitions.

Spawns each configured stdio MCP server, performs MCP handshake,
calls tools/list, and writes the combined result.

Also injects built-in Codex deferred tools (multi_agent_v1) that are not
served by any MCP server but need to be present in the LB cache so the
backend can see them.

Usage:
  python3 scripts/generate_mcp_tools.py [out.json] [config.toml]
"""

import json, os, subprocess, sys, time


# ---------------------------------------------------------------------------
# Static definitions for Codex built-in deferred tools
# These are tools that Codex CLI marks as ToolExposure::Deferred and only
# exposes via the client-side tool_search mechanism.  Since the LB converts
# tool_search into a regular function call that the backend cannot handle,
# we must inject these definitions directly so the backend always sees them.
# ---------------------------------------------------------------------------

COLLAB_INPUT_ITEMS_SCHEMA = {
    "type": "array",
    "description": "Structured input items. Use this to pass explicit mentions (for example app:// connector paths).",
    "items": {
        "type": "object",
        "properties": {
            "type": {"type": "string", "description": "Input item type: text, image, local_image, skill, or mention."},
            "text": {"type": "string", "description": "Text content when type is text."},
            "image_url": {"type": "string", "description": "Image URL when type is image."},
            "path": {"type": "string", "description": "Path when type is local_image/skill, or structured mention target such as app://<connector-id> or plugin://<plugin-name>@<marketplace-name> when type is mention."},
            "name": {"type": "string", "description": "Display name when type is skill or mention."},
        },
    },
}


MULTI_AGENT_V1_TOOLS = {
    "multi_agent_v1": {
        "namespace": "multi_agent_v1",
        "tools": [
            {
                "name": "spawn_agent",
                "description": (
                    "Spawn a sub-agent for a well-scoped task. Returns the spawned agent id "
                    "plus the user-facing nickname when available. Spawned agents inherit your "
                    "current model by default. Omit `model` to use that preferred default; set "
                    "`model` only when an explicit override is needed.\n\n"
                    "Do not spawn sub-agents unless the user explicitly asks for sub-agents, delegation, "
                    "or parallel agent work.\n"
                    "Requests for depth, thoroughness, research, investigation, or detailed codebase "
                    "analysis do not count as permission to spawn.\n\n"
                    "### When to delegate vs. do the subtask yourself\n"
                    "- Use a subagent when a subtask is easy enough for it to handle and can run in "
                    "parallel with your local work. Prefer delegating concrete, bounded sidecar tasks "
                    "that materially advance the main task without blocking your immediate next local step.\n"
                    "- Do not delegate urgent blocking work when your immediate next step depends on "
                    "that result.\n"
                    "- Keep work local when the subtask is too difficult to delegate well.\n\n"
                    "### Designing delegated subtasks\n"
                    "- Subtasks must be concrete, well-defined, and self-contained.\n"
                    "- Delegated subtasks must materially advance the main task.\n"
                    "- Do not duplicate work between the main rollout and delegated subtasks.\n"
                    "- For coding tasks, prefer delegating concrete code-change worker subtasks.\n\n"
                    "### After you delegate\n"
                    "- Call wait_agent very sparingly. Only call wait_agent when you need the result "
                    "immediately for the next critical-path step and you are blocked until it returns.\n"
                    "- Do not redo delegated subagent tasks yourself; focus on integrating results.\n"
                    "- While the subagent is running in the background, do meaningful non-overlapping "
                    "work immediately.\n\n"
                    "### Parallel delegation patterns\n"
                    "- Run multiple independent information-seeking subtasks in parallel when you have "
                    "distinct questions that can be answered independently.\n"
                    "- Split implementation into disjoint codebase slices and spawn multiple agents for "
                    "them in parallel when the write scopes do not overlap."
                ),
                "parameters": {
                    "type": "object",
                    "properties": {
                        "message": {"type": "string", "description": "Initial plain-text task for the new agent. Use either message or items."},
                        "items": COLLAB_INPUT_ITEMS_SCHEMA,
                        "agent_type": {"type": "string", "description": "Agent role for the new agent."},
                        "fork_context": {"type": "boolean", "description": "True forks the current thread history into the new agent; false or omitted starts with only the initial prompt."},
                        "model": {"type": "string", "description": "Model override for the new agent. Omit unless an explicit override is needed."},
                        "reasoning_effort": {"type": "string", "description": "Reasoning effort override for the new agent. Omit to inherit the parent effort."},
                        "service_tier": {"type": "string", "description": "Service tier override for the new agent. Omit unless explicitly requested."},
                    },
                },
            },
            {
                "name": "send_input",
                "description": "Send a message to an existing agent. Use interrupt=true to redirect work immediately. You should reuse the agent by send_input if you believe your assigned task is highly dependent on the context of a previous task.",
                "parameters": {
                    "type": "object",
                    "properties": {
                        "target": {"type": "string", "description": "Agent id to message (from spawn_agent)."},
                        "message": {"type": "string", "description": "Legacy plain-text message to send to the agent. Use either message or items."},
                        "items": COLLAB_INPUT_ITEMS_SCHEMA,
                        "interrupt": {"type": "boolean", "description": "True interrupts the current task and handles this message immediately; false or omitted queues it."},
                    },
                    "required": ["target"],
                },
            },
            {
                "name": "resume_agent",
                "description": "Resume a previously closed agent by id so it can receive send_input and wait_agent calls.",
                "parameters": {
                    "type": "object",
                    "properties": {
                        "id": {"type": "string", "description": "Agent id to resume."},
                    },
                    "required": ["id"],
                },
            },
            {
                "name": "wait_agent",
                "description": "Wait for agents to reach a final status. Completed statuses may include the agent's final message. Returns empty status when timed out. Once the agent reaches a final status, a notification message will be received containing the same completed status.",
                "parameters": {
                    "type": "object",
                    "properties": {
                        "targets": {
                            "type": "array",
                            "items": {"type": "string"},
                            "description": "Agent ids to wait on. Pass multiple ids to wait for whichever finishes first.",
                        },
                        "timeout_ms": {"type": "number", "description": "Timeout in milliseconds. Defaults to 600000, min 10000, max 3600000. Prefer longer waits (minutes) to avoid busy polling."},
                    },
                    "required": ["targets"],
                },
            },
            {
                "name": "close_agent",
                "description": "Close an agent and any open descendants when they are no longer needed, and return the target agent's previous status before shutdown was requested. Completed agents remain open and count toward the concurrency limit until closed. Don't keep agents open for too long if they are not needed anymore.",
                "parameters": {
                    "type": "object",
                    "properties": {
                        "target": {"type": "string", "description": "Agent id to close (from spawn_agent)."},
                    },
                    "required": ["target"],
                },
            },
        ],
    },
}




def parse_toml_mcp_servers(path):
    servers = {}
    current = None
    in_env = False
    with open(path) as f:
        for line in f:
            line = line.strip()
            if not line or line.startswith("#"):
                continue
            if line.startswith("[") and line.endswith("]"):
                rest = line[1:-1]
                if rest.startswith("mcp_servers."):
                    name = rest[len("mcp_servers."):]
                    if "." not in name:
                        current = name
                        in_env = False
                        servers[current] = {"args": [], "env": {}}
                    elif name == current + ".env" if current else False:
                        in_env = True
                    elif current and name.startswith(current + "."):
                        in_env = False
                    else:
                        current = None
                        in_env = False
                else:
                    current = None
                    in_env = False
                continue
            if current is None or "=" not in line:
                continue
            key, val = line.split("=", 1)
            key = key.strip()
            val = val.strip().strip('"').strip("'")
            if in_env:
                servers[current]["env"][key] = val
            elif key == "command":
                servers[current]["command"] = val
            elif key == "args":
                try:
                    servers[current]["args"] = json.loads(val)
                except Exception:
                    pass
    return {k: v for k, v in servers.items() if v.get("command")}


def discover_tools(server_name, cfg, timeout=15):
    cmd = [cfg["command"]] + cfg.get("args", [])
    env = os.environ.copy()
    env.update(cfg.get("env", {}))
    try:
        proc = subprocess.Popen(
            cmd, stdin=subprocess.PIPE, stdout=subprocess.PIPE,
            stderr=subprocess.PIPE, text=True, env=env
        )
    except Exception as e:
        print(f"  {server_name}: spawn failed -- {e}", file=sys.stderr)
        return None

    deadline = time.time() + timeout

    def can_read(fd, remaining):
        import select
        r, _, _ = select.select([fd], [], [], max(0, remaining))
        return bool(r)

    def read_line(fd, remaining):
        if remaining <= 0:
            return None
        import select
        r, _, _ = select.select([fd], [], [], min(remaining, 1.0))
        if r:
            return fd.readline()
        return None

    def send(obj):
        try:
            proc.stdin.write(json.dumps(obj) + "\n")
            proc.stdin.flush()
        except Exception:
            pass

    time.sleep(2.0)
    while time.time() < deadline:
        line = read_line(proc.stderr, deadline - time.time())
        if line is None:
            break

    send({"jsonrpc": "2.0", "id": 1, "method": "initialize",
          "params": {"protocolVersion": "2024-11-05", "capabilities": {},
                     "clientInfo": {"name": "mcp-gen", "version": "1.0.1"}}})

    initialized = False
    while time.time() < deadline:
        line = read_line(proc.stdout, deadline - time.time())
        if line is None:
            break
        try:
            resp = json.loads(line)
        except json.JSONDecodeError:
            continue
        if resp.get("id") == 1:
            initialized = True
            break

    if not initialized:
        print(f"  {server_name}: init timeout", file=sys.stderr)
        proc.kill()
        return None

    send({"jsonrpc": "2.0", "method": "notifications/initialized"})
    send({"jsonrpc": "2.0", "id": 3, "method": "tools/list", "params": {}})

    tools = None
    while time.time() < deadline:
        line = read_line(proc.stdout, deadline - time.time())
        if line is None:
            break
        try:
            resp = json.loads(line)
        except json.JSONDecodeError:
            continue
        if resp.get("id") == 3 and "result" in resp:
            tools = resp["result"].get("tools", [])
            break

    proc.stdin.close()
    try:
        proc.wait(timeout=3)
    except subprocess.TimeoutExpired:
        proc.kill()

    return tools


def main():
    out_path = sys.argv[1] if len(sys.argv) > 1 else "mcp_tools.json"
    config_path = sys.argv[2] if len(sys.argv) > 2 else os.path.expanduser("~/.codex/config.toml")

    if not os.path.exists(config_path):
        print(f"Config not found: {config_path}", file=sys.stderr)
        sys.exit(1)

    servers = parse_toml_mcp_servers(config_path)
    if not servers:
        print("No MCP servers found", file=sys.stderr)
        sys.exit(1)

    print(f"Found {len(servers)} servers: {list(servers.keys())}", file=sys.stderr)

    result = {}
    for name, cfg in servers.items():
        print(f"Discovering {name} ({cfg['command']})...", file=sys.stderr)
        tools = discover_tools(name, cfg)
        if not tools:
            print(f"  {name}: FAILED", file=sys.stderr)
            continue

        namespace = f"mcp__{name}"
        tool_defs = []
        for t in tools:
            tool_defs.append({
                "name": t.get("name", "?"),
                "description": t.get("description", ""),
                "parameters": t.get("inputSchema", {"type": "object", "properties": {}}),
            })
        result[name] = {"namespace": namespace, "tools": tool_defs}
        print(f"  {name}: {len(tool_defs)} tools OK", file=sys.stderr)

    # Inject built-in Codex deferred tools (not from MCP servers)
    for key, entry in MULTI_AGENT_V1_TOOLS.items():
        if key not in result:
            result[key] = entry
            print(f"  {key}: {len(entry['tools'])} tools injected (built-in deferred)", file=sys.stderr)


    if not result:
        print("No tools discovered from any server", file=sys.stderr)
        sys.exit(1)

    with open(out_path, "w") as f:
        json.dump(result, f, indent=2)
    print(f"Wrote {out_path} ({len(result)} namespaces, {sum(len(v['tools']) for v in result.values())} tools)", file=sys.stderr)


if __name__ == "__main__":
    main()
