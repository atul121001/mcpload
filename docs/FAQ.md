# Common questions

[README](../README.md) · [All docs](README.md)

**Do I need to write test scripts?**
No. The ready-made scenarios cover the common cases, and you control them with flags and `--env` settings. If you want a custom traffic pattern, you can write your own scenario in JavaScript (see [Write your own scenarios](guide/advanced.md#write-your-own-scenarios-javascript); mcpload's engine is k6, so k6 experience carries over).

**Will it break my server?**
It can, if you push it hard. That's the point of a load test, so use staging and start with a few agents. Be careful with tools that change data: limit the test with `TOOL_MIX`.

**Is it safe to point mcpload at production?**
Only with the owner's approval, and carefully: a load test is meant to push a server until it struggles. Prefer staging. If you must test production, limit `TOOL_MIX` to read-only tools (by default mcpload calls every tool the server lists), use a few agents for a short time outside peak hours, and leave out `--chaos-restart`. See the [threat model](THREAT_MODEL.md#5-load-testing-is-an-action-against-the-target).

**Does it send my data anywhere?**
No. Everything runs on your machine or your CI runner. Reports stay local unless you pass `--upload-url` to send one to a server you choose. Tool arguments and results aren't stored in reports. The load engine's anonymous usage report (k6's, to Grafana) is turned off. Details: [threat model](THREAT_MODEL.md).

**Which MCP servers does it work with?**
Remote MCP servers over streamable HTTP, in any language. It has been tested against servers built with the official TypeScript, Python and Go SDKs. Local stdio servers aren't supported.

**How long should a soak test be?**
30–60 minutes catches most slow leaks, and 10 minutes of steady load is a sensible minimum. A few minutes is enough to check that everything is wired up, but leak checks are skipped below 2 minutes of steady load, and a short soak only catches fast leaks.

**Does it run a real LLM?**
No. The agents follow scripted plans, so runs are repeatable and cost nothing. Pauses stand in for the time an agent spends thinking, and answers to sampling requests are canned replies with a delay you choose. Your real agents may call tools in a different order, so write your own plan with `--env WORKFLOW=...` if the default doesn't look like your traffic.

**How is it different from JMeter or other k6 MCP extensions?**
See [How it compares](COMPARISON.md). In short: those give you an MCP client to build tests with; mcpload ships the agent traffic model (parallel calls, multi-step plans, sampling answers) and the verdicts on top of it.
