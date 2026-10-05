# Security policy

## Supported versions

mcpload is pre-1.0. Security fixes go into the latest minor release only (currently **0.4.x**) and ship as a new patch or minor release. Older versions don't get backports, so upgrade to the latest release to pick up a fix (`install.sh` / `install.ps1` upgrade in place, or pin `ghcr.io/atul121001/mcpload:<version>`).

## Reporting a vulnerability

Please **don't open a public issue** for a security problem.

Report it privately through GitHub instead: on [github.com/atul121001/mcpload](https://github.com/atul121001/mcpload), open the **Security** tab and use the **Report a vulnerability** button. This opens a private security advisory that only you and the maintainers can see.

Helpful details:

- the mcpload version (`mcpload version`) and how you installed it (release archive, install script, Homebrew, Docker image, GitHub Action);
- what an attacker needs (for example: control of the MCP server under test, write access to a folder, a malicious pull request);
- steps or a proof of concept, and what you expected to happen instead.

## What to expect

mcpload is a small open-source project maintained on a best-effort basis, so there is no guaranteed response time. We will acknowledge your report in the advisory as soon as we can, work out a fix and a disclosure date with you there, and credit you in the advisory and release notes unless you'd rather stay anonymous. Please give us a reasonable chance to release a fix before you disclose the issue publicly.

## Scope

In scope:

- the `mcpload` CLI (`cmd/mcpload`), including the engine it embeds and extracts, the HTML report and report uploads;
- the k6 extension `xk6-mcpload` (the MCP wire client: auth, server-to-client requests, response handling);
- the bundled scenarios (`scenarios/`);
- the GitHub Action (`action/`, also published as `atul121001/mcpload-action`);
- the install scripts (`install.sh`, `install.ps1`) and the Homebrew formula (`packaging/homebrew/`);
- the Docker image `ghcr.io/atul121001/mcpload` and its `Dockerfile`.

Out of scope:

- **The demo servers** in `demo-servers/` and the `ghcr.io/atul121001/mcpload-demo-*` images that `mcpload demo` runs. They are deliberately broken and vulnerable test targets (memory leaks, misconfigured load balancers, a mock OAuth server with public credentials), bound to `127.0.0.1`. Problems that only matter when they are exposed to a network are expected.
- Load you generate yourself: mcpload is a load-testing tool, and slowing down or crashing the server you point it at is what it is for. See [Using mcpload safely](#using-mcpload-safely).
- Vulnerabilities in k6, Go, Docker or GitHub Actions themselves (report those upstream), unless mcpload uses them in an unsafe way.

## Using mcpload safely

The [threat model](docs/THREAT_MODEL.md) explains what data goes where and what mcpload trusts. In short:

- **Only test servers you own or have written permission to test.** Start with staging, not production, and with a few agents.
- **Limit the tools it calls.** By default mcpload calls *every* tool the server lists, with placeholder arguments. If any tool changes data, set `TOOL_MIX` to safe, read-only tools.
- **Keep secrets out of command lines and URLs.** Export `MCP_TOKEN`, `OAUTH_CLIENT_SECRET` and `MCPLOAD_KEY` as environment variables instead of passing them with `--env` / `--key`, where other local users can see them in the process list. In the GitHub Action, pass them from `secrets` in the `env` input (or `api-key`), never in `url` or `extra-args`.
- **Treat reports as shareable, not secret, but review them before you publish.** `report.json` and `report.html` don't contain tokens, tool arguments or tool results, but they do contain the target URL (with credentials redacted), tool names, timings and the git commit.
- **Use `--chaos-restart` and `--sampler docker` only on a machine and containers you control.** Both need access to the Docker daemon, which is root-equivalent.
- **Pin versions in CI** (`atul121001/mcpload/action@v0.5.0`, `ghcr.io/atul121001/mcpload:0.5.0`, `MCPLOAD_VERSION=v0.5.0` for the install scripts) and don't run the Action from `pull_request_target` on untrusted pull requests.
- **Never expose the demo servers** to a network.
