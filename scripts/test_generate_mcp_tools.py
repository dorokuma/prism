#!/usr/bin/env python3
"""Unit tests for generate_mcp_tools.py's TOML parsing.

Runs standalone:  python3 scripts/test_generate_mcp_tools.py
(no dependencies beyond the Python 3.11+ standard library).
"""

import importlib.util
import os
import subprocess
import sys
import tempfile
import unittest

HERE = os.path.dirname(os.path.abspath(__file__))
SCRIPT = os.path.join(HERE, "generate_mcp_tools.py")

spec = importlib.util.spec_from_file_location("generate_mcp_tools", SCRIPT)
gen = importlib.util.module_from_spec(spec)
spec.loader.exec_module(gen)


COMPLEX_TOML = r'''
# comment line
model = "gpt-5"

[mcp_servers.filesystem]
command = "/usr/local/bin/mcp-fs"
args = ["--root", "/tmp/root dir", "--flag=true"]
env = { LOG_LEVEL = "debug", PATH = "/usr/bin:/bin" }

[mcp_servers.web]
command = 'python3'
args = ["-m", "mcp_server.web", "a b c"]
env.PYTHONUNBUFFERED = "1"
env.API_KEY = "k=secret&x=1"

[mcp_servers.multiline]
command = "/bin/echo"
description = """
  multi
  line
"""

[mcp_servers.no_command]
args = ["nothing"]

[unrelated.section]
value = 1
'''


class ParseTomlMcpServersTest(unittest.TestCase):
    def test_complex_toml_parsed(self):
        with tempfile.NamedTemporaryFile("w", suffix=".toml", delete=False) as f:
            f.write(COMPLEX_TOML)
            path = f.name
        try:
            servers = gen.parse_toml_mcp_servers(path)
        finally:
            os.unlink(path)

        # filesystem: array args (with spaces/quotes), inline env table.
        self.assertIn("filesystem", servers)
        self.assertEqual(servers["filesystem"]["command"], "/usr/local/bin/mcp-fs")
        self.assertEqual(servers["filesystem"]["args"], ["--root", "/tmp/root dir", "--flag=true"])
        self.assertEqual(servers["filesystem"]["env"], {"LOG_LEVEL": "debug", "PATH": "/usr/bin:/bin"})

        # web: single-quoted command, dotted env keys.
        self.assertIn("web", servers)
        self.assertEqual(servers["web"]["command"], "python3")
        self.assertEqual(servers["web"]["args"], ["-m", "mcp_server.web", "a b c"])
        self.assertEqual(servers["web"]["env"], {"PYTHONUNBUFFERED": "1", "API_KEY": "k=secret&x=1"})

        # multiline strings are legal TOML and must not break parsing.
        self.assertIn("multiline", servers)
        self.assertEqual(servers["multiline"]["command"], "/bin/echo")
        self.assertEqual(servers["multiline"]["args"], [])

        # A table without a command is skipped; unrelated sections ignored.
        self.assertNotIn("no_command", servers)

    def test_bad_toml_raises(self):
        with tempfile.NamedTemporaryFile("w", suffix=".toml", delete=False) as f:
            f.write("[mcp_servers.broken]\ncommand = \"x\"\nargs = [unclosed\n")
            path = f.name
        try:
            with self.assertRaises(Exception) as ctx:
                gen.parse_toml_mcp_servers(path)
            # tomllib.TOMLDecodeError is a subclass of ValueError.
            self.assertIsInstance(ctx.exception, ValueError)
        finally:
            os.unlink(path)

    def test_empty_and_missing_sections(self):
        with tempfile.NamedTemporaryFile("w", suffix=".toml", delete=False) as f:
            f.write('other = "value"\n')
            path = f.name
        try:
            self.assertEqual(gen.parse_toml_mcp_servers(path), {})
        finally:
            os.unlink(path)

    def test_main_fails_fast_on_bad_toml(self):
        """The CLI must exit non-zero on a malformed TOML (fail fast)."""
        with tempfile.NamedTemporaryFile("w", suffix=".toml", delete=False) as f:
            f.write("[mcp_servers.broken]\ncommand = \"x\"\nargs = [unclosed\n")
            bad = f.name
        out_path = tempfile.mktemp(suffix=".json")
        try:
            proc = subprocess.run(
                [sys.executable, SCRIPT, out_path, bad],
                capture_output=True, text=True,
            )
            self.assertNotEqual(proc.returncode, 0, "bad TOML must exit non-zero")
            self.assertIn("Invalid TOML", proc.stderr)
            self.assertFalse(os.path.exists(out_path),
                             "no output must be written on a bad TOML")
        finally:
            os.unlink(bad)
            if os.path.exists(out_path):
                os.unlink(out_path)


if __name__ == "__main__":
    unittest.main()
