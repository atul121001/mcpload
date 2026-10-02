"""mcpload demo target: official Python MCP SDK (mcp 2.x) in stateless streamable-HTTP mode.

As of mcp 2.2.0 (30 Sep 2026) the high-level server is MCPServer (mcp.server.mcpserver), formerly FastMCP; same
high-level API. Served with uvicorn on 0.0.0.0:$PORT at /mcp.

Env: PORT (3000), FLAKY_RATE (0.1), BIG_BYTES (200000), JSON_RESPONSE (0).
"""

from __future__ import annotations

import asyncio
import json
import os
import random
import resource
import socket
from urllib.parse import quote

import uvicorn
from starlette.requests import Request
from starlette.responses import JSONResponse, PlainTextResponse

from mcp.server.mcpserver import MCPServer

PORT = int(os.environ.get("PORT", "3000"))
FLAKY_RATE = float(os.environ.get("FLAKY_RATE", "0.1"))
BIG_BYTES = int(os.environ.get("BIG_BYTES", "200000"))
JSON_RESPONSE = os.environ.get("JSON_RESPONSE", "0") == "1"
REPLICA = socket.gethostname()


def make_big_text(n: int) -> str:
    line = "mcpload big payload 0123456789 abcdefghijklmnopqrstuvwxyz ABCDEFGHIJKLMNOPQRSTUVWXYZ\n"
    return (line * (n // len(line) + 1))[:n]


BIG_TEXT = make_big_text(BIG_BYTES)

mcp = MCPServer(name="py-healthy", version="0.1.0")


class FlakyError(Exception):
    pass


@mcp.tool(description="Returns immediately.", structured_output=False)
def fast() -> str:
    return "ok"


@mcp.tool(description="Sleeps for `ms` milliseconds (default 300) before returning.", structured_output=False)
async def slow(ms: int = 300) -> str:
    await asyncio.sleep(max(0, min(ms, 120_000)) / 1000)
    return f"slept {ms}ms"


@mcp.tool(description=f"Returns isError:true with probability `rate` (default {FLAKY_RATE}).", structured_output=False)
def flaky(rate: float | None = None) -> str:
    p = FLAKY_RATE if rate is None else rate
    if random.random() < p:
        # MCPServer turns tool exceptions into CallToolResult(isError=True).
        raise FlakyError("flaky: simulated tool failure")
    return "flaky: ok"


@mcp.tool(description=f"Returns a large text payload (default {BIG_BYTES} bytes).", structured_output=False)
def big(bytes: int | None = None) -> str:
    if bytes is None or bytes == BIG_BYTES:
        return BIG_TEXT
    return make_big_text(max(1, min(bytes, 10_000_000)))


@mcp.tool(description="Echoes the query with a short fake result list.", structured_output=False)
def search(query: str, limit: int = 5) -> str:
    n = max(1, min(limit, 50))
    results = [
        {
            "title": f'Result {i + 1} for "{query}"',
            "url": f"https://example.com/search/{quote(query)}/{i + 1}",
            "score": round(1 - i / (n + 1), 3),
        }
        for i in range(n)
    ]
    return json.dumps({"query": query, "results": results})


def _rss_bytes() -> int:
    try:
        with open("/proc/self/status") as f:
            for line in f:
                if line.startswith("VmRSS:"):
                    return int(line.split()[1]) * 1024
    except OSError:
        pass
    return resource.getrusage(resource.RUSAGE_SELF).ru_maxrss * 1024


@mcp.custom_route("/healthz", methods=["GET"])
async def healthz(_: Request) -> JSONResponse:
    return JSONResponse({"ok": True, "name": "py-healthy", "replica": REPLICA})


@mcp.custom_route("/metrics", methods=["GET"])
async def metrics(_: Request) -> PlainTextResponse:
    body = (
        "# HELP process_resident_memory_bytes Resident memory size in bytes.\n"
        "# TYPE process_resident_memory_bytes gauge\n"
        f"process_resident_memory_bytes {_rss_bytes()}\n"
        "# HELP mcp_active_sessions Sessions held by the server (always 0: stateless).\n"
        "# TYPE mcp_active_sessions gauge\n"
        "mcp_active_sessions 0\n"
    )
    return PlainTextResponse(body, media_type="text/plain; version=0.0.4")


# host="0.0.0.0" so the SDK does not enable localhost-only DNS-rebinding protection
# (requests arrive via Docker port mapping with Host: localhost:3003 from a non-loopback peer).
app = mcp.streamable_http_app(
    streamable_http_path="/mcp",
    stateless_http=True,
    json_response=JSON_RESPONSE,
    host="0.0.0.0",
)

if __name__ == "__main__":
    uvicorn.run(app, host="0.0.0.0", port=PORT, log_level="warning")
