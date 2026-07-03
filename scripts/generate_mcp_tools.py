#!/usr/bin/env python3
"""Generate mcp_tools.json from ~/.codex/config.toml MCP server definitions.

Spawns each configured MCP server, performs the MCP stdio handshake,
calls tools/list, and writes the combined result to mcp_tools.json.

Usage:
  python3 scripts/generate_mcp_tools.py [--out mcp_tools.json] [--config ~/.codex/config.toml]
"""

import json, os, subprocess, sys, time


def parse_toml_mcp_servers(path):
    """Extract MCP server entries from a Codex config.toml.

    Returns {server_name: {command, args, env}}
    """
    servers = {}
    current = None
    with open(path) as f:
        for line in f:
            line = line.strip()
            # [mcp_servers.NAME]
            if line.startswith("[mcp_servers.") and line.endswith("]"):
                name = line[len("[mcp_servers."):-1]
                # skip sub-sections like .tools.XXX or .env
                if "." in name:
                    continue
                current = name
                servers[name] = {"args": [], "env": {}}
                continue
            if current is None:
                continue
            if line.startswith("[") and line.endswith("]"):
                # new top-level section
                if not line.startswith(f"[mcp_servers.{current}."):
                    current = None
                continue
            if "=" in line:
                key, val = line.split("=", 1)
                key = key.strip()
                val = val.strip().strip('"').strip("'")
                if key == "command":
                    servers[current]["command"] = val
                # args and env are handled in subsections; skip inline
    # Remove entries without command
    return {k: v for k, v in servers.items() if v.get("command")}


def mcp_handshake_and_list(proc):
    """Perform MCP initialize + tools/list over stdio. Returns tool list or None."""
    def send(msg):
        proc.stdin.write(json.dumps(msg) + "\n")
        proc.stdin.flush()

    def recv():
        line = proc.stdout.readline()
        if not line:
            return None
        try:
            return json.loads(line.strip())
        except json.JSONDecodeError:
            return None

    send({"jsonrpc": "2.0", "id": 1, "method": "initialize",
          "params": {"protocolVersion": "2024-11-05", "capabilities": {},
                     "clientInfo": {"name": "mcp-tools-gen", "version": "1.0.0"}}})

    # Read initialize response (may include a notification before it)
    for _ in range(10):
        resp = recv()
        if resp is None:
            return None
        if resp.get("id") == 1:
            break

    # Send initialized notification
    send({"jsonrpc": "2.0", "method": "notifications/initialized"})

    # Send tools/list
    send({"jsonrpc": "2.0", "id": 3, "method": "tools/list", "params": {}})

    for _ in range(10):
        resp = recv()
        if resp is None:
            return None
        if resp.get("id") == 3:
            return resp.get("result", {}).get("tools", [])
    return None


def discover_tools(server_name, cfg, timeout=15):
    """Spawn an MCP server and discover its tools."""
    cmd = [cfg["command"]] + cfg.get("args", [])
    try:
        proc = subprocess.Popen(
            cmd, stdin=subprocess.PIPE, stdout=subprocess.PIPE,
            stderr=subprocess.PIPE, text=True
        )
        tools = mcp_handshake_and_list(proc)
        proc.stdin.close()
        proc.wait(timeout=3)
        return tools
    except Exception as e:
        print(f"  {server_name}: error — {e}", file=sys.stderr)
        return None


def main():
    out_path = sys.argv[1] if len(sys.argv) > 1 else "mcp_tools.json"
    config_path = sys.argv[2] if len(sys.argv) > 2 else os.path.expanduser("~/.codex/config.toml")

    if not os.path.exists(config_path):
        print(f"Config not found: {config_path}", file=sys.stderr)
        # fallback: read existing mcp_tools.json if it exists
        if os.path.exists(out_path):
            print(f"Keeping existing {out_path}", file=sys.stderr)
            sys.exit(0)
        sys.exit(1)

    servers = parse_toml_mcp_servers(config_path)
    if not servers:
        print("No MCP servers found in config", file=sys.stderr)
        sys.exit(1)

    result = {}
    for name, cfg in servers.items():
        print(f"Discovering {name}...", file=sys.stderr)
        tools = discover_tools(name, cfg)
        if tools is None or len(tools) == 0:
            print(f"  {name}: no tools discovered, skipping", file=sys.stderr)
            continue

        # Derive namespace from server name
        namespace = f"mcp__{name}"

        tool_defs = []
        for t in tools:
            tool_defs.append({
                "name": t.get("name", "?"),
                "description": t.get("description", ""),
                "parameters": t.get("inputSchema", {"type": "object", "properties": {}}),
            })

        result[name] = {
            "namespace": namespace,
            "tools": tool_defs,
        }
        print(f"  {name}: {len(tool_defs)} tools", file=sys.stderr)

    with open(out_path, "w") as f:
        json.dump(result, f, indent=2)
    print(f"Wrote {out_path} ({len(result)} namespaces)", file=sys.stderr)


if __name__ == "__main__":
    main()
