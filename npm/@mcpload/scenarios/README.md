# @atulmishra121001/scenarios

> k6 scenario library for [mcpload](https://github.com/atul121001/mcpload) — MCP load & soak testing for AI agents

Publish the mcpload scenario library on npm so you can import and extend them in your own k6 scripts.

## Install

```bash
npm install @atulmishra121001/scenarios
```

## Usage

### Run with k6 (built with xk6-mcpload)

```bash
# Use a scenario directly
k6 run node_modules/@atulmishra121001/scenarios/agent-session.js

# Or import and extend
import { agentSession, makeClient } from '@atulmishra121001/scenarios/lib/session.js';
import { config } from '@atulmishra121001/scenarios/lib/config.js';

export default function () {
  const client = makeClient();
  agentSession(client);
}
```

### Available Scenarios

| Scenario | Description |
|----------|-------------|
| `agent-session` | Default: parallel tool calls, session management |
| `agent-workflow` | Chained tool calls (builds next from last result) |
| `burst` | Burst load testing (initialize flood) |
| `soak` | Long-duration soak tests (leak detection) |
| `step-load` | Stepped load increase (capacity analysis) |
| `isolation` | Session isolation testing |
| `lb-check` | Load balancer "session not found" testing |
| `long-lived` | Long-lived connection testing |
| `oauth-refresh` | OAuth token refresh testing |
| `reconnect-storm` | Reconnection storm simulation |
| `version-skew` | Version skew testing (rolling deploys) |
| `workload` | Custom workload profiles |

### Available Library Modules

| Module | Description |
|--------|-------------|
| `lib/config` | Configuration helpers (env vars, thresholds) |
| `lib/session` | Agent session logic (`agentSession`, `makeClient`) |
| `lib/tools` | Tool table planning and weighted random picks |
| `lib/cancel` | Cancellation options |
| `lib/resilience` | Resilience testing helpers |
| `lib/schema-args` | Generate args from tool input schemas |
| `lib/skew` | Version skew helpers |
| `lib/steps` | Step-load helpers |
| `lib/workflow` | Multi-step workflow planning |
| `lib/workload` | Custom workload profiles |

## Example: Custom Scenario

```javascript
// custom-scenario.js
import { agentSession, makeClient } from '@atulmishra121001/scenarios/lib/session.js';
import { config, buildThresholds, env, envNum } from '@atulmishra121001/scenarios/lib/config.js';

const client = makeClient();

export const options = {
  scenarios: {
    agents: {
      executor: 'constant-vus',
      vus: envNum('VUS', 20),
      duration: env('DURATION', '5m'),
    },
  },
  thresholds: buildThresholds(),
};

export default function () {
  agentSession(client);
}
```

```bash
k6 run custom-scenario.js -e MCP_URL=http://localhost:3001/mcp
```

## License

Apache-2.0 — [LICENSE](https://github.com/atul121001/mcpload/blob/main/LICENSE)

## Related

- [@atulmishra121001/cli](https://www.npmjs.com/package/@atulmishra121001/cli) — npm wrapper for the mcpload CLI
- [mcpload](https://github.com/atul121001/mcpload) — The Go binary and full documentation
