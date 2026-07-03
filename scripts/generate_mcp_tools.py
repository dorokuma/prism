#!/usr/bin/env python3
"""Generate mcp_tools.json from ~/.codex/config.toml MCP server definitions.

Spawns each configured stdio MCP server, performs MCP handshake,
calls tools/list, and writes the combined result.

Usage:
  python3 scripts/generate_mcp_tools.py [out.json] [config.toml]
"""

import json, os, subprocess, sys, time


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
                        # sub-section like tools.X or env.X — skip
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
        print(f"  {server_name}: spawn failed — {e}", file=sys.stderr)
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

    # Wait for startup (npx may take several seconds)
    time.sleep(2.0)
    while time.time() < deadline:
        line = read_line(proc.stderr, deadline - time.time())
        if line is None:
            break

    # Send initialize
    send({"jsonrpc": "2.0", "id": 1, "method": "initialize",
          "params": {"protocolVersion": "2024-11-05", "capabilities": {},
                     "clientInfo": {"name": "mcp-gen", "version": "1.0.0"}}})

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

    if not result:
        print("No tools discovered from any server", file=sys.stderr)
        sys.exit(1)

    with open(out_path, "w") as f:
        json.dump(result, f, indent=2)
    print(f"Wrote {out_path} ({len(result)} namespaces, {sum(len(v['tools']) for v in result.values())} tools)", file=sys.stderr)


if __name__ == "__main__":
    main()
