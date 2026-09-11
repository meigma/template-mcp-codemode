#!/usr/bin/env python3
# /// script
# requires-python = ">=3.11"
# ///
"""Drive a real MCP session against a built server and assert CodeMode behavior.

This is the release smoke test: it speaks the actual stdio JSON-RPC protocol to a
server binary (or container) and exercises the full CodeMode loop — discover,
describe, execute — instead of only probing `--version`/`--help`. Anything that
breaks the worker re-exec, the capability catalog, or the Starlark execution path
fails here rather than in a user's client.

The target is passed as a literal argv after `--`, so the same script covers every
release artifact:

    uv run .github/scripts/mcp_smoke.py -- ./bin/template-mcp-codemode stdio
    uv run .github/scripts/mcp_smoke.py -- dist/release-assets/BINARY stdio
    uv run .github/scripts/mcp_smoke.py -- docker run -i --rm IMAGE stdio

The server identity assertion is exact, so pointing the script at the dev proxy
(`mcp-devproxy`) or at the bare CodeMode library server (`codemode`) is reported as
a failure rather than silently passing.
"""

from __future__ import annotations

import argparse
import json
import queue
import subprocess
import sys
import threading
from typing import Any

# EXPECTED_TOOLS is the exact CodeMode tool surface: no more, no fewer.
EXPECTED_TOOLS = frozenset({"search_api", "describe_api", "execute"})

# DETERMINISTIC_PROGRAM composes several capability calls whose results are pinned by
# min == max, so a passing run proves real execution rather than a lucky random draw.
DETERMINISTIC_PROGRAM = """
def main():
    total = 0
    for _ in range(3):
        total = total + {capability}(min=7, max=7)["value"]
    return {{"total": total, "single": {capability}(min=-2, max=-2)["value"]}}
"""

# DETERMINISTIC_RESULT is the only correct envelope for DETERMINISTIC_PROGRAM.
DETERMINISTIC_RESULT = {"result": {"total": 21, "single": -2}}

# INVALID_RANGE_PROGRAM asks for an impossible range, which the capability must reject.
INVALID_RANGE_PROGRAM = """
def main():
    return {capability}(min=5, max=1)
"""


class SmokeError(RuntimeError):
    """Raised when the smoke test fails; the message is the failure reason."""


def parse_args(argv: list[str]) -> tuple[argparse.Namespace, list[str]]:
    """Split argv on the first `--` so the target argv keeps its own flags."""
    if "--" in argv:
        index = argv.index("--")
        own, command = argv[:index], argv[index + 1 :]
    else:
        own, command = argv, []

    parser = argparse.ArgumentParser(
        description=__doc__,
        formatter_class=argparse.RawDescriptionHelpFormatter,
        usage="%(prog)s [options] -- COMMAND [ARG ...]",
    )
    parser.add_argument(
        "--server-name",
        default="template-mcp-codemode",
        help="exact initialize serverInfo.name the target must report",
    )
    parser.add_argument(
        "--capability",
        default="random.int",
        help="exact capability name to discover, describe, and execute",
    )
    parser.add_argument(
        "--expect-error-text",
        default="capability failed",
        help="exact tool error text for the rejected out-of-range call",
    )
    parser.add_argument(
        "--protocol-version",
        # The newest version the legacy `initialize` handshake can negotiate: the
        # go-sdk caps initialize at 2025-11-25 because 2026-07-28 replaces it with
        # `discover`. The negotiated version the server answers with is accepted
        # either way, so a newer server does not break this check.
        default="2025-11-25",
        help="protocol version requested at initialize",
    )
    parser.add_argument(
        "--timeout",
        default=60.0,
        type=float,
        help="seconds to wait for any single response",
    )
    args = parser.parse_args(own)
    if not command:
        parser.error("a target argv is required after `--`")
    return args, command


def main(argv: list[str] | None = None) -> int:
    args, command = parse_args(sys.argv[1:] if argv is None else argv)
    try:
        run_smoke(
            command=command,
            server_name=args.server_name,
            capability=args.capability,
            expect_error_text=args.expect_error_text,
            protocol_version=args.protocol_version,
            timeout=args.timeout,
        )
    except SmokeError as exc:
        print(f"error: {exc}", file=sys.stderr)
        return 1
    print("[smoke] PASS")
    return 0


def run_smoke(
    *,
    command: list[str],
    server_name: str,
    capability: str,
    expect_error_text: str,
    protocol_version: str,
    timeout: float,
) -> None:
    print(f"[smoke] target: {' '.join(command)}")
    with Session(command, timeout=timeout) as session:
        check_initialize(session, server_name=server_name, protocol_version=protocol_version)
        check_tools(session)
        signature = check_search(session, capability=capability)
        check_describe(session, capability=capability, signature=signature)
        check_execute(session, capability=capability)
        check_invalid_range(session, capability=capability, expect_error_text=expect_error_text)
        # The rejected call runs in its own worker process; a healthy server keeps
        # serving afterwards. Re-running the deterministic program proves it.
        check_execute(session, capability=capability)


def check_initialize(session: Session, *, server_name: str, protocol_version: str) -> None:
    result = session.request(
        "initialize",
        {
            "protocolVersion": protocol_version,
            "capabilities": {},
            "clientInfo": {"name": "mcp-smoke", "version": "1"},
        },
    )
    negotiated = result.get("protocolVersion")
    if not isinstance(negotiated, str) or not negotiated:
        raise SmokeError(f"initialize returned no protocol version: {result}")

    info = result.get("serverInfo")
    if not isinstance(info, dict):
        raise SmokeError(f"initialize returned no serverInfo: {result}")
    actual = info.get("name")
    if actual != server_name:
        raise SmokeError(f"serverInfo.name is {actual!r}, want exactly {server_name!r}")
    if not isinstance(result.get("capabilities"), dict) or "tools" not in result["capabilities"]:
        raise SmokeError(f"server does not advertise the tools capability: {result}")

    session.notify("notifications/initialized", {})
    print(
        f"[smoke] initialize: {actual} "
        f"{info.get('version', '?')} (protocol {negotiated})"
    )


def check_tools(session: Session) -> None:
    result = session.request("tools/list", {})
    tools = result.get("tools")
    if not isinstance(tools, list):
        raise SmokeError(f"tools/list returned no tools array: {result}")
    names = {tool.get("name") for tool in tools if isinstance(tool, dict)}
    if names != set(EXPECTED_TOOLS):
        raise SmokeError(f"tools are {sorted(map(str, names))}, want {sorted(EXPECTED_TOOLS)}")
    print(f"[smoke] tools/list: {sorted(EXPECTED_TOOLS)}")


def check_search(session: Session, *, capability: str) -> str:
    payload = session.call_tool("search_api", {"query": capability})
    results = payload.get("results")
    if not isinstance(results, list):
        raise SmokeError(f"search_api returned no results array: {payload}")
    matches = [
        entry
        for entry in results
        if isinstance(entry, dict) and entry.get("name") == capability
    ]
    if not matches:
        found = sorted(
            str(entry.get("name")) for entry in results if isinstance(entry, dict)
        )
        raise SmokeError(f"search_api did not return {capability!r}; got {found}")
    signature = matches[0].get("signature")
    if not isinstance(signature, str) or not signature.startswith(capability + "("):
        raise SmokeError(f"search_api signature for {capability!r} is {signature!r}")
    print(f"[smoke] search_api: {signature}")
    return signature


def check_describe(session: Session, *, capability: str, signature: str) -> None:
    payload = session.call_tool("describe_api", {"name": capability})
    if payload.get("name") != capability:
        raise SmokeError(f"describe_api returned {payload.get('name')!r}, want {capability!r}")
    if payload.get("signature") != signature:
        raise SmokeError(
            f"describe_api signature {payload.get('signature')!r} "
            f"disagrees with search_api {signature!r}"
        )
    inputs = field_names(payload, "input")
    outputs = field_names(payload, "output")
    if not {"min", "max"} <= inputs:
        raise SmokeError(f"describe_api input fields are {sorted(inputs)}, want min and max")
    if "value" not in outputs:
        raise SmokeError(f"describe_api output fields are {sorted(outputs)}, want value")
    print(f"[smoke] describe_api: input {sorted(inputs)} output {sorted(outputs)}")


def check_execute(session: Session, *, capability: str) -> None:
    program = DETERMINISTIC_PROGRAM.format(capability=capability)
    payload = session.call_tool("execute", {"source": program})
    if payload != DETERMINISTIC_RESULT:
        raise SmokeError(f"execute returned {payload}, want {DETERMINISTIC_RESULT}")
    print(f"[smoke] execute: {payload}")


def check_invalid_range(session: Session, *, capability: str, expect_error_text: str) -> None:
    program = INVALID_RANGE_PROGRAM.format(capability=capability)
    result = session.request("tools/call", {"name": "execute", "arguments": {"source": program}})
    if not result.get("isError"):
        raise SmokeError(f"execute accepted an impossible range: {result}")
    text = result_text(result)
    if text != expect_error_text:
        raise SmokeError(f"execute error text is {text!r}, want exactly {expect_error_text!r}")
    print(f"[smoke] execute rejected min>max: {text}")


def field_names(payload: dict[str, Any], key: str) -> set[str]:
    fields = payload.get(key)
    if not isinstance(fields, list):
        raise SmokeError(f"describe_api returned no {key} array: {payload}")
    return {
        field["name"]
        for field in fields
        if isinstance(field, dict) and isinstance(field.get("name"), str)
    }


def result_text(result: dict[str, Any]) -> str:
    blocks = result.get("content")
    if not isinstance(blocks, list):
        return ""
    texts = [
        block["text"]
        for block in blocks
        if isinstance(block, dict) and isinstance(block.get("text"), str)
    ]
    return "\n".join(texts).strip()


class Session:
    """One MCP stdio session: newline-delimited JSON-RPC over a child process."""

    def __init__(self, command: list[str], *, timeout: float) -> None:
        self._command = command
        self._timeout = timeout
        self._next_id = 0
        try:
            self._process = subprocess.Popen(  # noqa: S603 - argv comes from the caller
                command,
                stdin=subprocess.PIPE,
                stdout=subprocess.PIPE,
                text=True,
                bufsize=1,
            )
        except OSError as exc:
            raise SmokeError(f"could not start {command[0]!r}: {exc}") from exc
        # A reader thread keeps every wait bounded: a hung or dead server surfaces as
        # a timeout or EOF instead of blocking the release job forever.
        self._lines: queue.Queue[str | None] = queue.Queue()
        self._reader = threading.Thread(target=self._read_lines, daemon=True)
        self._reader.start()

    def __enter__(self) -> Session:
        return self

    def __exit__(self, *_: object) -> None:
        self.close()

    def request(self, method: str, params: dict[str, Any]) -> dict[str, Any]:
        self._next_id += 1
        request_id = self._next_id
        self._send({"jsonrpc": "2.0", "id": request_id, "method": method, "params": params})
        while True:
            message = self._receive()
            if message.get("id") != request_id:
                # Server-initiated requests and notifications are not part of this
                # smoke test; skip anything that is not our response.
                continue
            if "error" in message:
                raise SmokeError(f"{method} failed: {message['error']}")
            result = message.get("result")
            if not isinstance(result, dict):
                raise SmokeError(f"{method} returned no result object: {message}")
            return result

    def notify(self, method: str, params: dict[str, Any]) -> None:
        self._send({"jsonrpc": "2.0", "method": method, "params": params})

    def call_tool(self, name: str, arguments: dict[str, Any]) -> dict[str, Any]:
        """Call one tool and return its structured content, failing on a tool error."""
        result = self.request("tools/call", {"name": name, "arguments": arguments})
        if result.get("isError"):
            raise SmokeError(f"{name} reported a tool error: {result_text(result)!r}")
        payload = result.get("structuredContent")
        if not isinstance(payload, dict):
            raise SmokeError(f"{name} returned no structured content: {result}")
        return payload

    def close(self) -> None:
        process = self._process
        if process.stdin is not None:
            try:
                process.stdin.close()
            except OSError:
                pass
        try:
            process.wait(timeout=self._timeout)
        except subprocess.TimeoutExpired:
            process.kill()
            process.wait()
            raise SmokeError("server did not exit after its stdin was closed") from None
        if process.returncode not in (0, -15):
            raise SmokeError(f"server exited with status {process.returncode}")

    def _send(self, message: dict[str, Any]) -> None:
        stdin = self._process.stdin
        if stdin is None:
            raise SmokeError("server stdin is not available")
        try:
            stdin.write(json.dumps(message) + "\n")
            stdin.flush()
        except OSError as exc:
            raise SmokeError(f"could not write to the server: {exc}") from exc

    def _receive(self) -> dict[str, Any]:
        try:
            line = self._lines.get(timeout=self._timeout)
        except queue.Empty:
            raise SmokeError(f"no response within {self._timeout:g}s") from None
        if line is None:
            status = self._process.poll()
            raise SmokeError(f"server closed stdout (exit status {status})")
        try:
            message = json.loads(line)
        except json.JSONDecodeError as exc:
            raise SmokeError(f"server wrote non-JSON to stdout: {line!r} ({exc})") from exc
        if not isinstance(message, dict):
            raise SmokeError(f"server wrote a non-object JSON-RPC message: {line!r}")
        return message

    def _read_lines(self) -> None:
        stdout = self._process.stdout
        if stdout is not None:
            for line in stdout:
                if line.strip():
                    self._lines.put(line)
        self._lines.put(None)


if __name__ == "__main__":
    raise SystemExit(main())
