# How mcpload compares

[README](../README.md) · [All docs](README.md)

Testing an MCP server means answering four different questions. Most tools answer one of them well. mcpload is built for the fourth one.

| The question | Tools built for it | What they don't tell you |
|---|---|---|
| **Does my server work?** One client, one request at a time. | [MCP Inspector](https://github.com/modelcontextprotocol/inspector), [MCPJam](https://www.mcpjam.com/) | What happens with 100 agents at once. |
| **Does the agent pick the right tool and finish the task?** | [mcp-eval](https://github.com/lastmile-ai/mcp-eval), [mcpbr](https://mcpbr.org/), agent eval platforms | Whether the server stays fast and up under load. |
| **How fast is one endpoint under load?** | k6, JMeter, Locust, Gatling, Artillery | Nothing about MCP: no sessions, tool lists or tool calls, so they can't reproduce agent traffic. |
| **Will it hold up when many real agents use it, for hours?** | **mcpload** | |

## Side by side with the other MCP load tools

Three other projects send MCP traffic under load. This table comes from each project's own README (checked October 2026). "Not in docs" means we couldn't find it documented, not that it can't be built on top.

| | **mcpload** | [JMeter MCP plugin](https://github.com/Blazemeter/jmeter-mcp-plugin) (BlazeMeter) | [xk6-mcp](https://github.com/dgzlopes/xk6-mcp) (k6) | [mcp-bench](https://pkg.go.dev/github.com/tmc/mcp/exp/cmd-experimental/mcp-bench) |
|---|---|---|---|---|
| **Status** | Early release (v0.5) | v0.1.0 | Experimental, "not officially supported by Grafana Labs" | Experimental Go command |
| **How you use it** | One command with ready-made scenarios | JMeter GUI test plan (Java 17+) | Write a k6 script | CLI |
| **MCP sessions** | One per simulated agent, each with its own session ID | **One shared client for the whole test run**; every thread uses the same session | One per client your script creates | Concurrent clients |
| **Several tool calls at once inside one session** | ✅ `callParallel` | ❌ synchronous client, one call per thread | ❌ `callTool` returns before the next call | Not in docs |
| **Next call built from the previous result** | ✅ `agent-workflow` scenario with `$from` references | Possible with JMeter extractors, by hand | Possible in your script, by hand | ❌ |
| **Weighted mix of business flows** | ✅ `--workload`: flows with weights, CSV test data and per-flow budgets in one YAML file | Throughput controllers and CSV data sets you wire up by hand | Possible in your script, by hand | ❌ |
| **Latency per tool** | ✅ p50/p95/p99 and error rate for every tool | ✅ one sampler per tool | ❌ metrics are tagged by `method` only, so every `tools/call` is mixed together | Not in docs |
| **Budget per tool, with PASS/FAIL** | ✅ `P95_MS`, `TOOL_BUDGETS` | With assertions you configure | ❌ no per-tool tag to set a threshold on | ❌ exports results, no pass/fail |
| **"How many agents can it take?"** | ✅ `mcpload capacity`: a step table with tail latency and error classes, "breaks budget: `slow` p95 1.26 s > 800 ms" at 20 agents, "Estimated sustainable capacity: ~13 agents" | Ramp threads and read the graphs yourself | Ramp VUs and read the graphs yourself | Stress mode that scales up |
| **Does one slow tool block the others?** | ✅ `isolation` scenario and verdict | ❌ | ❌ | ❌ |
| **Leak detection** | ✅ memory, sessions and open files; slope over steady load plus a cool-down check; Docker or Prometheus | ❌ | ❌ | Watches memory, goroutines and GC of the server process |
| **Load balancer "session not found"** | ✅ dedicated scenario and verdict | ❌ one shared session can't reproduce it | ❌ | ❌ |
| **Cost of opening sessions (initialize floods)** | ✅ `burst` scenario, connect time measured on every session | ❌ connect and `initialize` are excluded from sample time and happen once | Measured if your script reconnects | Not in docs |
| **Sampling / elicitation requests from the server** | ✅ answered with a configurable delay | Not in docs | Not in docs | Not in docs |
| **Rolling deploy: replicas on different builds** | ✅ `version-skew`: fail-fast vs hang verdict | ❌ | ❌ | ❌ |
| **Server restart mid-run: recovery time** | ✅ `--chaos-restart` and `recovery` verdict | ❌ | ❌ | ❌ |
| **Lost or duplicated tool calls** | ✅ `call_integrity`: what the client saw vs what the server ran | ❌ | ❌ | ❌ |
| **Cancellation: does the server stop the work?** | ✅ `cancellation` verdict from server metrics | Not in docs | Not in docs | Not in docs |
| **Long-lived sessions dropped early** | ✅ `long-lived` scenario and `session_survival` verdict | ❌ | ❌ | ❌ |
| **Auth** | Bearer token, OAuth client credentials with shared refresh | Not in docs | Not in docs | Not in docs |
| **Stateless 2026-07-28 protocol** | ✅ auto-detected | Not in docs | Not in docs | Not in docs |
| **CI** | ✅ GitHub Action with a PR comment and HTML report | `jmeter -n` plus assertions | `k6 run` plus thresholds | Export to other tools |
| **Compare with main: per-tool Δ** | ✅ `mcpload compare` / `baseline-branch: main`: p95/p99/error rate per tool vs the last `main` run, with noise floors | ❌ | ❌ | ❌ |
| **Resources and prompts** | ✅ `readResource` / `getPrompt`, tagged per resource and prompt; mixed into agent sessions with `RESOURCE_READ_RATIO` / `PROMPT_GET_RATIO` ([scenarios](../scenarios/README.md)) | ✅ | ✅ | Not in docs |
| **stdio and SSE servers** | ❌ streamable HTTP only | ✅ | ✅ | ✅ |

## What that means in practice

- **Session bugs only show up with many sessions.** The JMeter plugin shares one MCP client across all threads, so 100 threads look like one very busy agent. It can't show you sessions piling up in memory, or a load balancer sending an agent's second request to a replica that never saw its session. mcpload gives every simulated agent its own session, so both show up.
- **Agents call tools in parallel.** A real agent fires off `search`, `fetch` and `fetch` at once and waits for all three. JMeter's synchronous client and xk6-mcp's `callTool` send one call at a time. mcpload's `callParallel` sends them together, which is what fills a shared connection pool or blocks an event loop.
- **"The server is slow" isn't actionable. "`search` is slow" is.** xk6-mcp tags its metrics only by `method`, so a 10 ms tool and a 2 s tool end up in the same number. mcpload reports and budgets every tool separately, and the CI check names the tool that broke.
- **The worst production failures happen during deploys and restarts.** A rolling deploy puts two builds behind one load balancer; a restart drops every in-flight call. mcpload tests both and tells you whether clients get a clear error or hang, how long recovery takes, and whether any tool call was lost or executed twice. The other tools only measure a server that stays up.
- **You get an answer, not a graph.** Every mcpload run ends with PASS or FAIL and a sentence saying why, such as "max sustainable concurrency: 10 agents (budgets broke at 20)" or "RSS grew 64 MiB/min and did not recover in cool-down". With the other tools you collect the numbers and decide yourself.

## When to use something else

- Use **MCP Inspector or MCPJam** to debug a single request.
- Use **mcp-eval or mcpbr** to check that agents pick the right tools.
- Use **the JMeter plugin or xk6-mcp** to load-test local stdio servers or SSE servers. mcpload supports only streamable HTTP so far.

These tools work well alongside mcpload.
