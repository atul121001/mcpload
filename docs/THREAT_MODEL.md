# Threat model

This document is for people who need to decide whether mcpload is safe to run in their environment: security reviewers, platform teams and careful users. It describes what mcpload touches, what data goes where, what is stored or logged, the defaults, and what we recommend. It also lists the [known limitations](#known-limitations) we haven't fixed.

It describes mcpload **v0.5.x**. To report a vulnerability, see [SECURITY.md](../SECURITY.md).

## Contents

- [The system in one picture](#the-system-in-one-picture)
- [Assets](#assets)
- [Trust boundaries and actors](#trust-boundaries-and-actors)
- [1. Tokens and credentials](#1-tokens-and-credentials)
- [2. Tool arguments, results and what a report contains](#2-tool-arguments-results-and-what-a-report-contains)
- [3. Report uploads and baselines](#3-report-uploads-and-baselines)
- [4. A malicious or compromised MCP server](#4-a-malicious-or-compromised-mcp-server)
- [5. Load testing is an action against the target](#5-load-testing-is-an-action-against-the-target)
- [6. Docker](#6-docker)
- [7. Chaos restarts](#7-chaos-restarts)
- [8. Install scripts](#8-install-scripts)
- [9. Embedded engine and scenario extraction](#9-embedded-engine-and-scenario-extraction)
- [10. GitHub Action](#10-github-action)
- [11. Demo servers](#11-demo-servers)
- [Known limitations](#known-limitations)

## The system in one picture

```text
  your machine or CI runner                              |  network
                                                         |
  mcpload CLI --starts--> engine (k6 + xk6-mcpload)      |
    |                       runs a scenario script ------+--> MCP server under test   (--url)
    |                       (JavaScript, sees the env) --+--> OAuth token endpoint    (OAUTH_TOKEN_URL)
    |-- --wait-ready probe ------------------------------+--> MCP server under test
    |-- --sampler prometheus ----------------------------+--> /metrics URL            (--prom-url)
    |-- --calls-url -------------------------------------+--> call-tracking endpoint  (--calls-url)
    |-- --baseline <URL> --------------------------------+--> any http(s) URL you give
    |-- --upload-url ------------------------------------+--> report upload server    (--upload-url)
    |-- --sampler docker / --chaos-restart --> docker CLI --> Docker daemon (local, or $DOCKER_HOST)
    '-- writes report.json, report.html (+ --k6-out raw metrics) to local disk

  GitHub Action only: builds from source, writes the job summary, posts the PR comment,
  uploads the report artifact, looks up and downloads the baseline artifact.
```

Nothing is sent anywhere except to the URLs you configure. mcpload has no telemetry and no update check. Its engine is k6, which on its own POSTs an anonymous usage report to Grafana (`stats.grafana.org`) after every run; mcpload starts the engine with `K6_NO_USAGE_REPORT=true` to turn that off, unless you set `K6_NO_USAGE_REPORT` yourself. (Other network use is explicit: `mcpload demo up` pulls images from GHCR, the install scripts download from GitHub, and the GitHub Action downloads Go modules to build from source.)

## Assets

| Asset | Where it lives | Why it matters |
|---|---|---|
| MCP credentials: `MCP_TOKEN`, `MCP_HEADERS` values (API keys), OAuth `OAUTH_CLIENT_ID` / `OAUTH_CLIENT_SECRET` and the access tokens fetched with them | your shell or CI environment, process memory of the engine | access to your MCP server and whatever it fronts |
| Upload API key (`MCPLOAD_KEY` / `--key`) | environment or command line | write access to your report server |
| Tool arguments and test data (`TOOL_ARGS`, workload profiles and their CSV files) | your files, process memory, the wire | may contain customer-like or real data |
| Tool results and server error messages | process memory, terminal output | may contain data from the system under test |
| Reports (`report.json`, `report.html`, `--k6-out` files) | local disk, CI artifacts, the upload server, the PR comment | describe your server: URL, tool names, latencies, commit |
| The MCP server under test and the data behind it | your infrastructure | a load test can slow it down, crash it or, through data-changing tools, modify data |
| The machine running mcpload | laptop, CI runner | runs the engine and, with Docker features, talks to the Docker daemon |
| The GitHub repository | GitHub | the Action can write PR comments with the job's token |

## Trust boundaries and actors

| Actor | Trusted? | Notes |
|---|---|---|
| You, the operator, and the files you pass (`--scenario`, `--workload`, `--baseline`, env) | trusted | A scenario script is code. Anything you pass on the command line is assumed to be intended. |
| The MCP server under test | **not trusted** for the client's safety | In CI it is often built from the pull request being tested, so its responses, tool names and error messages are attacker-controllable whenever the PR is. See [section 4](#4-a-malicious-or-compromised-mcp-server). |
| The OAuth token endpoint, the Prometheus endpoint, the `--calls-url` endpoint | trusted for their own purpose | They get credentials (token endpoint) or influence verdicts. |
| The upload server (`--upload-url`) | trusted with report contents and the API key | |
| The baseline report (`--baseline`, `baseline-branch`) | trusted for numbers | It sets what counts as a regression. Its strings appear in the PR comment. |
| Other local users on the same machine | not trusted | They can read process command lines, and anything you put in shared folders. |
| GitHub release assets, GHCR images, raw.githubusercontent.com | trusted origin | The install scripts and Homebrew formula verify integrity against checksums from the same release. See [section 8](#8-install-scripts). |
| The Docker daemon | fully trusted, root-equivalent | Only used for `--sampler docker`, `--chaos-restart` and `mcpload demo`. |

---

## 1. Tokens and credentials

**How credentials flow**

- `MCP_TOKEN` becomes a static `Authorization: Bearer <token>` header on every request to the MCP server, including answers to server-to-client requests and the session `DELETE`.
- With `OAUTH_TOKEN_URL` set (it wins over `MCP_TOKEN`), the engine fetches a token with the OAuth 2.0 `client_credentials` grant from that URL, sending the client id and secret as HTTP Basic auth (or in the form body with `authStyle: 'post'` in a custom script). The access token is cached in memory, shared by all virtual users with the same credentials, and refreshed in the background shortly before it expires; it is never written to disk.
- `MCP_HEADERS` (a JSON object) is added to every MCP request as is. The `--wait-ready` probe also sends `MCP_HEADERS` and the `MCP_TOKEN` bearer header; it does not fetch OAuth tokens.
- The engine's HTTP client uses k6's transport, so k6's TLS settings apply. TLS verification is on unless you turn it off yourself (for example with k6's `--insecure-skip-tls-verify` in a custom setup).

**Where credentials are not written.** No token, client secret, header value or upload key is written to `report.json`, `report.html`, the k6 NDJSON metrics (`--k6-out`), the job summary or the PR comment. Metric samples carry only the tags `method`, `tool`, `resource`, `prompt`, `protocol`, `status` and `error_type` plus scenario tags such as `flow`, `step` and `replica`; a `resource` tag built from a URI keeps only its scheme, host and first path segment, never user info, query or fragment. Error messages that mcpload builds never include header values; a failed token fetch reports the HTTP status and up to 200 bytes of the token endpoint's response body, which is the identity provider's own error text.

**URLs.** A credential in a URL is different: `--url`, `--baseline` and `--calls-url` are recorded in the report, and `--url` is logged. mcpload replaces the password in `user:password@` and the values of query parameters whose names look like credentials (`token`, `key`, `secret`, `sig`, `signature`, `credential`, `auth`, `password`, `api_key`, `X-Amz-Signature`, ...) with `REDACTED` in `report.json` and in its own log lines. Two places still print the URL as you gave it: the scenario's own start-up line in the k6 output (`agent-session -> <url>`), and the command line the GitHub Action echoes. **Don't put secrets in URLs.**

**Command lines are visible to other local users.** `--env MCP_TOKEN=...` puts the token in mcpload's command line, and mcpload passes every `--env` value to the engine as `-e KEY=VALUE`, so the value also appears in the engine's command line. Any user on the same machine can see command lines (`ps`, `/proc/<pid>/cmdline`, Task Manager), and your shell may keep them in its history. The same is true of `--key` for uploads.

The engine also reads the OS environment (`k6 run` passes it to scripts by default, and mcpload's `k6 inspect` call adds `--include-system-env-vars` to match), so **exporting the variable keeps it off every command line**:

```bash
export MCP_TOKEN=...            # or OAUTH_CLIENT_SECRET, MCPLOAD_KEY
mcpload run --url https://staging.example.com/mcp
```

The flip side: every scenario script can read every environment variable of the mcpload process. Only run scenario scripts you trust (see [section 9](#9-embedded-engine-and-scenario-extraction)).

**Upload key.** `--upload-url` reads the key from `--key` or, if that is empty, from `$MCPLOAD_KEY`. Prefer the environment variable. The GitHub Action passes `api-key` to `mcpload upload` through `MCPLOAD_KEY`, not on the command line, and registers it with `::add-mask::`.

**GitHub Action.** Lines of the `env` input are passed to mcpload as `--env` arguments; the Action echoes the command line with every value replaced by `***` (`--env MCP_TOKEN=***`). GitHub also masks values that come from `secrets`. Values you type literally into the workflow file are not secrets and aren't masked anywhere else. `url`, `label` and `extra-args` are echoed as given, which is why the Action's input descriptions say not to put secrets there. On a self-hosted runner shared with other users, the command-line caveat above applies to `env` values too.

**Redirects.** If the MCP server answers with an HTTP redirect, the engine follows it (Go's default: up to 10 redirects; 307 and 308 re-send the request body). Go drops `Authorization` when the redirect goes to a different host that isn't a subdomain of the original one, but it keeps it for the same host even when the redirect downgrades `https` to `http`, and it forwards custom headers from `MCP_HEADERS` (an `X-API-Key`, say) to **any** host. The `--wait-ready` probe, `--baseline` and `--upload-url` requests follow redirects the same way. See [known limitations](#known-limitations).

**Recommendations**

- Export credentials as environment variables; avoid `--env SECRET=...` and `--key` on shared machines and in shell history.
- Use a dedicated, least-privilege token or OAuth client for load tests, scoped to staging, and rotate it if a report or log leaks.
- Use `https://` URLs for the MCP server, the token endpoint and the upload server. Make sure the server doesn't redirect, or that it only redirects within the same host and scheme.
- In CI, pass credentials from `secrets` in the `env` input; never in `url` or `extra-args`.

## 2. Tool arguments, results and what a report contains

**Arguments.** Each tool call's arguments come from `TOOL_ARGS`, from the demo arguments (only on the demo servers), or from placeholders that mcpload generates from the tool's `inputSchema`. With a workload profile, they come from the profile and its data pools (inline values or CSV files). Arguments are sent to the MCP server and nowhere else.

**Workload data.** `mcpload run --workload` reads the profile and its CSV files, writes a normalized copy (including the data rows) to a temporary file created with mode `0600` in the system temp folder for the engine to read, and deletes it when the run ends. A run killed with `SIGKILL` can leave that file behind.

**Results.** Tool results and error objects are returned to the scenario script, which needs them (the `agent-workflow` and `workload` scenarios pass values from one call's result into the next call's arguments). They are not attached to metric samples and not written to any report. The bundled scenarios print the first 20 failed calls per virtual user to the terminal, including the server's error message (for example `search: http: <message>`). HTTP error bodies are read up to 64 KiB; the message is the JSON-RPC error message when the body has one, otherwise the body shortened to 300 characters.

**`--include-payloads` / `includePayloads`.** The flag is accepted and recorded as `payloadsIncluded: true` in the report, and it sets `INCLUDE_PAYLOADS=1` for the scenario, but the extension doesn't act on it yet: payloads are never stored, whatever its value.

**What `report.json` contains** (and therefore `report.html`, an upload, and the PR comment summary):

- the run: id, start and end time, duration, scenario name, protocol version, k6 version, load shape, load-generator CPU usage;
- the target URL (credentials redacted, see [section 1](#1-tokens-and-credentials)) and your `--label`;
- the git commit and ref when given (`--git-sha`, `--git-ref`, or `$GITHUB_SHA` / `$GITHUB_REF` in CI);
- per-tool request counts, error counts by class (`http`, `timeout`, `auth`, ...), latency percentiles and time series. **Tool names come from the server's `tools/list`**;
- thresholds (k6 expressions from the scenario), verdicts with a one-line message (which can name tools, flows and steps), server memory/session/fd series from the sampler;
- with a workload profile: its name, description, flow and step names, weights and budgets (not its data);
- with `--chaos-restart`: the container name and, on failure, the `docker restart` error output; with `--calls-url`: that URL (redacted) and counts of call ids; with `--baseline`: the baseline's source path or URL (redacted) and its summary numbers;
- not: tokens, headers, tool arguments, tool results, server error messages, environment variables.

Server-provided strings that can reach a report are therefore tool names and, in the `version-skew` verdict, replica names (from the `SERVED_BY_HEADER` response header, default `X-Served-By`). Treat a report like other build output: fine to share inside the team that owns the server, worth a look before you publish it.

**Raw engine output.** By default the k6 NDJSON metric stream and summary are written to a private temporary folder and deleted after the run. `--k6-out <dir>` keeps them; that folder is created with mode `0755`. They contain the same metric samples and tags as above.

## 3. Report uploads and baselines

**`--upload-url` / `mcpload upload`**

- **What is sent:** the whole `report.json`, exactly as written to disk, as one `POST {upload-url}/api/v1/runs` with `Content-Type: application/json`, `Authorization: Bearer <key>` and `User-Agent: mcpload/<version>`. `mcpload run` doesn't upload a report that fails its own schema checks.
- **Transport:** whatever scheme you give. `http://` is accepted, which sends the key and the report in clear text. Redirects are followed (see [section 1](#1-tokens-and-credentials)).
- **Response:** up to 1 MiB is read. On success it is printed to stdout and, if it has a `url`, that URL is logged (`uploaded run <id> (status <s>): <url>`). The Action passes that URL to the job summary as a link.
- **Idempotency:** none. There is no idempotency key and no retry; running `mcpload upload` twice sends the report twice. The report's `run.id` (a random UUID) is the natural key for the server to deduplicate on.
- **Timeouts:** 60 s per request, 2 min overall.

**`--baseline` and `mcpload compare`**

- A baseline is a local path or any `http://` or `https://` URL. URLs are fetched with a plain `GET` (no credentials are sent, so a private baseline URL needs its own access token in the URL, which is redacted in the report), a 30 s timeout, redirects followed, and at most 256 MiB decoded. The result must pass the same schema checks as `mcpload validate` before it is used.
- A baseline only provides numbers to compare against. A crafted baseline can make a run pass or fail the regression check, and its tool names appear in the comparison table of the HTML report and the PR comment, so use baselines from a source you trust (the Action's `baseline-branch` does this for you, see [section 10](#10-github-action)).

**Recommendations:** use `https://` for upload servers and baseline URLs; give the upload key only the right to create runs.

## 4. A malicious or compromised MCP server

The MCP server under test is the component an attacker is most likely to control: a compromised staging server, a third-party server you were asked to evaluate, or, in CI, a server built from a malicious pull request. This is what it can and can't do to the machine running mcpload.

**Memory and time (denial of service against the load generator).**

- A JSON response body is read in full, and an SSE response stream is read until the response with the right id arrives, with no size limit on either. The only bound is the request timeout: **30 s by default** (`MCP_TIMEOUT`, or `timeout` in a script; `0` disables it). Within that window a fast, hostile server can make the engine buffer a very large response.
- A slow or hanging response (slow-loris) costs one virtual user its request until the same timeout ends it. After the matching response arrives, the rest of an SSE stream is drained in the background for at most 1 MiB or 2 s.
- Bodies of error responses (non-2xx) are capped at 64 KiB, token endpoint responses at 1 MiB, readiness probe responses at 1 MiB, and `--calls-url` responses at 64 MiB.
- Each server-to-client request read from a stream is answered in its own goroutine, with no cap on how many a stream may carry.

Worst case, the load generator runs out of memory or slows down. The run then fails or its numbers are wrong; nothing leaves the machine.

**Server-to-client requests (sampling, elicitation, roots).** mcpload never calls an LLM, never asks a human and never runs a callback. Answers are fixed when the client is created:

- They are sent only when you enable them (`SAMPLING`, `ELICITATION`, `ROOTS`, or the same client options in a script); only then is the capability declared in `initialize`. Unknown methods get JSON-RPC `-32601`; `ping` gets `{}`.
- Sampling answers with a canned message (`"mcpload mock response"` by default).
- **Elicitation set to `1` answers `accept`** (with empty content) to every request. If a tool asks for confirmation before doing something destructive, mcpload will confirm it. Configure `{"action": "decline"}` where that matters.
- Roots answers with an empty list by default, or exactly the list you configure. mcpload never looks at your file system to build it.
- Answers are POSTed only to the MCP URL you configured, never to a URL the server chooses. With the 2026-07-28 (stateless) protocol, server-to-client requests on a response stream are not answered at all.

**Redirects to other hosts.** See [section 1](#1-tokens-and-credentials): the server already receives your credentials, but a redirect can forward custom headers (and, on a same-host `http` downgrade, the bearer token) to a third party or over clear text.

**HTML report (XSS).** Every string from the report is inserted into `report.html` through an HTML-escaping function (`&`, `<`, `>`, `"`, `'`), the report JSON is embedded in a `<script type="application/json">` block with `<`, `>` and `&` escaped so it can't close the tag, and the page title is stripped of `<`, `>` and `&`. The page loads no external scripts, styles or fonts. We tested this by putting `"><img src=x onerror=...>` into every string of the example reports: none appeared unescaped.

**PR comment and job summary (Markdown injection).** The Action's summary escapes `|` and newlines in table cells, but not other Markdown. A tool name containing a backtick can end the code span it is printed in and add links, images, `@mentions` or the HTML that GitHub allows to the comment posted by the workflow's token. GitHub's sanitizer removes scripts, so this is content spoofing, not code execution. See [known limitations](#known-limitations).

**Terminal output.** mcpload prints tool names (in tables and verdicts) and the bundled scenarios print server error messages without removing control characters, so a hostile server can put terminal escape sequences into your terminal or CI log.

**Calls to destructive tools.** By default mcpload calls **every tool the server lists**, uniformly, with placeholder arguments built from each tool's input schema. Tool annotations such as `destructiveHint` or `readOnlyHint` are not consulted. A server can therefore steer the test into calling any tool it lists, and an unfiltered run against a server with a `delete_*` or `send_*` tool will call it, repeatedly. Set `TOOL_MIX` to the tools you mean to test:

```bash
mcpload run --url https://staging.example.com/mcp --env 'TOOL_MIX={"search":5,"get_document":3}'
```

A name in `TOOL_MIX` that the server doesn't list is skipped with a warning and a failed check.

## 5. Load testing is an action against the target

- **Authorization.** Only test servers you own or have written permission to test. A load test against someone else's service can be indistinguishable from a denial-of-service attack, and the target's terms of service, your contract or the law may forbid it.
- **Staging first.** The point of the tool is to push a server until it degrades. Run against staging, start with a few agents (`--vus 5`), and raise the load gradually. `mcpload capacity` steps up the load on purpose; `step-load` stops itself once errors cross `ABORT_ERR_RATE`, but only after the server has already started failing.
- **Data-changing tools.** Use `TOOL_MIX` (section 4). Watch `TOOL_ARGS` and workload data: the same arguments are sent many times, so a call that creates a record creates thousands.
- **Shared dependencies.** A staging MCP server may share a database, an identity provider or a third-party API quota with production. Token refresh tests (`oauth-refresh`) deliberately put load on the token endpoint.
- **Chaos restarts** stop a container mid-run; see [section 7](#7-chaos-restarts).
- **Is it safe to point at production?** Only with the owner's explicit approval, a read-only `TOOL_MIX`, a low agent count and a short duration, and preferably outside peak hours. A soak or capacity test against production is a production incident you scheduled yourself.

## 6. Docker

**The image** `ghcr.io/atul121001/mcpload` is Alpine-based and contains mcpload, its engine and the bundled scenarios in `/opt/mcpload`. Its entrypoint is `mcpload`, its working directory is `/work`, and it sets `MCPLOAD_ENGINE=/opt/mcpload/k6` so the image always runs its own engine, never a file named `k6` in the folder you mount.

- **It runs as root** (the Dockerfile has no `USER`). Files written to a mounted folder are owned by root unless you add `--user "$(id -u):$(id -g)"`, which also limits what the container can touch in that folder. The image works as a non-root user.
- **Volume mounts.** `-v "$PWD:/work"` gives the container read-write access to that folder (and root's access, without `--user`). Mount a dedicated reports folder rather than your home directory or a repository with secrets in it. Note that a bare scenario name is looked up in `/work/scenarios/<name>.js` first (see [section 9](#9-embedded-engine-and-scenario-extraction)).
- **`--network host`** (Linux) shares the host's network namespace: the container can reach every service listening on the host's `127.0.0.1`, not just the server under test. Use it only to test local servers such as the demo servers; for a server on all interfaces, prefer `--add-host=host.docker.internal:host-gateway`, and for remote servers you need neither.
- **No Docker socket.** The image contains no `docker` command, so `--sampler docker` and `--chaos-restart` don't work inside it, and you should not mount `/var/run/docker.sock` into it. Use `--sampler prometheus` from a container.

**`--sampler docker` on the host** runs `docker stats --no-stream --format "{{json .}}" <container>` every interval and reads only the memory usage. Access to the Docker daemon is root-equivalent on Linux (a user who can run `docker` can start a privileged container), so only run it where you already have that access. The sampler talks to whichever daemon your `docker` CLI is configured for (`DOCKER_HOST`, the current context).

**Argument handling.** mcpload starts the `docker` binary directly with separate arguments (`docker`, `stats`, ..., `<container>`; `docker`, `restart`, `<container>`), never through a shell, so a container name can't inject shell commands. It doesn't add a `--` separator, so a value that starts with `-` would be read by `docker` as an option; only pass container names you chose.

`mcpload demo` runs `docker compose` with a compose file embedded in mcpload (print it with `mcpload demo config`).

## 7. Chaos restarts

`--chaos-restart <duration>` runs `docker restart <container>` once, that long after the load starts.

- **Never on by default.** The default is `0` (off). It needs an explicitly named container, `--chaos-container` (or `--container`); without one, mcpload refuses to start. `--chaos-container` without `--chaos-restart` is also an error. Only that one container is touched, once, and the restart is bounded by a 2-minute timeout.
- **No interactive prompt.** Before the run mcpload logs ``chaos: will run `docker restart <name>` <duration> after k6 starts. Only do this to servers you own.``, logs again when it restarts, and records the action, the container name and the outcome in `report.json`. Scripts and CI run it without confirmation, so review the command line.
- **Permissions.** It needs the same Docker daemon access as `--sampler docker` (root-equivalent). The restart drops every in-flight request and any in-memory state of that container.
- **Recommendation:** run a private copy of your server for chaos tests (the [demo servers README](../demo-servers/README.md#call-tracking-and-chaos-restarts-ts-image-off-in-compose) shows how), never a container that other people or production traffic depend on.

## 8. Install scripts

`install.sh` (Linux, macOS) and `install.ps1` (Windows) do the following:

1. Pick the release: `$MCPLOAD_VERSION`, or the latest release from the GitHub API (falling back to the `releases/latest` redirect when the API is rate-limited).
2. Download the archive for your OS and CPU and the release's `checksums.txt` over HTTPS from `github.com/atul121001/mcpload/releases/download/<tag>/`.
3. Compute the archive's SHA-256 and compare it with the entry in `checksums.txt`. A missing entry or a mismatch stops the install. `install.sh` refuses to continue when neither `sha256sum` nor `shasum` exists.
4. Unpack into a versioned folder and remove older versions installed there:
   - `install.sh`: `~/.mcpload/<tag>/` (or `$MCPLOAD_INSTALL_DIR`), with a `current` symlink, and a symlink `mcpload` in `~/.local/bin` (or `/usr/local/bin` if `~/.local/bin` isn't on your `PATH` and `/usr/local/bin` is, and is writable without `sudo`; or `$MCPLOAD_BIN_DIR`). On macOS it removes the `com.apple.quarantine` attribute from the unpacked files so Gatekeeper doesn't block them. It doesn't edit your shell profile; if the folder isn't on `PATH` it prints the line to add.
   - `install.ps1`: `%LOCALAPPDATA%\mcpload\<tag>\` (or `$env:MCPLOAD_INSTALL_DIR`), and adds that folder to your **user** `PATH` (replacing older mcpload entries) and to the current session's `PATH`.
   - `MCPLOAD_NO_PATH=1` skips the `PATH` and link changes.
5. Run `mcpload version` to check the binary works.

Neither script uses `sudo` or administrator rights, and neither runs anything as another user.

**What the checksum protects.** `checksums.txt` comes from the same GitHub release as the archive. The check catches a corrupted or truncated download and tampering between GitHub and you; it does **not** protect against a compromised release, because an attacker who can replace the archive can replace `checksums.txt` too. Release assets are not signed (no Sigstore/cosign signatures or GitHub artifact attestations as of v0.5.0). The Homebrew formula pins the same SHA-256 values.

**Piping a script into a shell** (`curl ... | sh`, `irm ... | iex`) runs whatever the server returns, from the `main` branch. Both scripts wrap everything in a function that is called on the last line, so a truncated download does nothing. If you'd rather look first:

```bash
curl -fsSLO https://raw.githubusercontent.com/atul121001/mcpload/main/install.sh
less install.sh
MCPLOAD_VERSION=v0.5.0 sh install.sh
```

```powershell
irm https://raw.githubusercontent.com/atul121001/mcpload/main/install.ps1 -OutFile install.ps1
notepad install.ps1
$env:MCPLOAD_VERSION = 'v0.5.0'; .\install.ps1
```

**Recommendations:** pin `MCPLOAD_VERSION` (or download the script from a release tag rather than `main`) so that a later release can't change what you install; in high-assurance environments, download the release archive yourself and compare its SHA-256 with a value you recorded out of band, or build from source.

## 9. Embedded engine and scenario extraction

Release builds of mcpload (`-tags embedengine`) carry the engine (a k6 build with xk6-mcpload) and the bundled scenarios. On first use they are extracted to the user cache folder:

| OS | Default location (`$MCPLOAD_CACHE_DIR` overrides it) |
|---|---|
| Linux | `$XDG_CACHE_HOME/mcpload`, or `~/.cache/mcpload` |
| macOS | `~/Library/Caches/mcpload` |
| Windows | `%LocalAppData%\mcpload` |

- The engine goes to `engine/<sha256 prefix>/mcpload-engine` and scenarios to `scenarios/<sha256 prefix>/`, each written to a temporary name and renamed into place, so a reader never sees a partial copy.
- **Verified on every reuse.** Before running the engine, mcpload checks the file's size and SHA-256 against the embedded copy, and before reusing extracted scenarios it compares every file with the embedded copy. A mismatch (corruption, or a file changed after extraction) is replaced by a fresh extraction.
- **Permissions.** Folders are created with mode `0755`, the engine with `0755` and scenario files with `0644` (minus your umask): readable by other users, writable only by you.
- **Not checked:** who owns the cache folder and whether others can write to it. There is also an unavoidable gap between the check and the moment the engine starts. Someone who can write to the cache folder could swap the engine in that window. The default locations are in your own account's folders, writable only by you; **never point `MCPLOAD_CACHE_DIR` at a shared or world-writable folder** such as `/tmp`.

**What else mcpload runs.**

- `--engine <path>` or `$MCPLOAD_ENGINE` runs that binary instead of the embedded engine.
- Development builds without the embedded engine (plain `go build`) look for `./k6` (`k6.exe` on Windows) in the **current folder** first, then next to the mcpload executable, then on `PATH`. Don't run a development build from a folder you don't trust. Release builds and the Docker image don't do this.
- **Scenario scripts are code.** A bare scenario name (and the default `agent-session`) is resolved as `scenarios/<name>.js` in the **current folder** first, then next to the executable, then among the embedded scenarios; mcpload prints the path it picked (`mcpload: using scenario ...`). A scenario runs inside k6's JavaScript runtime: it can't start processes, but it can read every environment variable of the process (including your tokens), read files when the script loads, and send HTTP requests anywhere. So running `mcpload run` inside an untrusted checkout that has a `scenarios/` folder runs that checkout's code with your credentials. Pass an explicit `--scenario` path, or run from a folder you trust.

## 10. GitHub Action

**Permissions.** Give the job only what it uses:

```yaml
permissions:
  contents: read        # actions/checkout
  pull-requests: write  # only with comment-on-pr: 'true'
  actions: read         # only with baseline-branch (find and download main's report artifact)
```

The Action uses `github-token` (default: the job's `GITHUB_TOKEN`) to list and update PR comments and to look up and download the baseline artifact.

**`pull_request_target`.** Don't run mcpload from `pull_request_target` (or `workflow_run`) on code from untrusted pull requests. Those events run with a write token and your secrets; if the workflow checks out the PR, builds its server and passes secrets in `env`, the PR's code (its server, a `scenarios/` folder, a `docker-compose.yml`) runs with them. Use `pull_request`: PRs from forks then get a read-only token and no secrets, so posting the comment fails and the Action logs a warning instead.

**Build from source.** The Action doesn't download release binaries. It builds the engine and the CLI from the action's own source at the ref you pinned (`uses: atul121001/mcpload/action@v0.5.0`), with `actions/setup-go` and `go install go.k6.io/xk6@<xk6-version>` (default `v1.4.14`); Go modules are fetched through the Go module proxy and verified against the Go checksum database. It doesn't pass a k6 version to `xk6 build`, so xk6 builds its default, the **latest** k6 release at build time (the release workflow and the Dockerfile pin k6 v2.3.0). The binaries are cached with `actions/cache` under a key that includes a hash of the source. Pin the action to a tag or, better, a full commit SHA.

**Secrets.** Put credentials in the `env` input from `secrets` (`MCP_TOKEN=${{ secrets.MCP_TOKEN }}`) and the upload key in `api-key`. See [section 1](#1-tokens-and-credentials) for how they are passed and masked. Never put them in `url`, `label` or `extra-args`, which are echoed to the log. GitHub masks secrets in logs only; the PR comment, the job summary and the report artifact are not masked, which is why mcpload redacts URL credentials in the report.

**Inputs reach shell steps through environment variables**, not `${{ }}` expressions inside `run:` scripts, so an input value (or a branch name) can't inject shell commands into the Action's own steps.

**Baseline trust (`baseline-branch`).** The Action compares with the report artifact of the newest **successful** run of the **same workflow** whose `head_branch` is the baseline branch and whose event is not `pull_request*` (so `push`, `schedule` or `workflow_dispatch`), checking at most 30 candidate runs. A fork's PR can't provide it, even from a branch named `main`, because PR runs are excluded. The baseline is therefore as trustworthy as the people who can push to that branch or trigger the workflow on it. With the `baseline` input, it is whatever path or URL you give.

**PR comment.** The comment is the Markdown summary of the report: target URL (redacted), scenario, per-tool table, verdicts and the comparison. It is created or updated by the job token, found by a hidden marker (`<!-- mcpload-report:<artifact-name> -->`). Tool names from the server are inserted without full Markdown escaping (see [section 4](#4-a-malicious-or-compromised-mcp-server)). The Action updates the first comment containing the marker, whoever wrote it.

**Artifact.** `report.json` and `report.html` are uploaded as a workflow artifact, readable by anyone who can read the repository's Actions runs (everyone, on a public repository).

## 11. Demo servers

The servers in `demo-servers/` and the `ghcr.io/atul121001/mcpload-demo-*` images run by `mcpload demo up` are **deliberately vulnerable and broken**: some leak memory until they are killed, the load balancers are misconfigured on purpose, `skew-hang` never answers some requests, and `mock-oauth` is not a real authorization server: its client credentials (`mcpload:secret`) are public, in the compose files.

- Every published port in both compose files (`demo-servers/docker-compose.yml` and the copy embedded in mcpload) is bound to `127.0.0.1`; `--base-port` only shifts the port numbers. Automatic restarts are off.
- Keep it that way: never change the bindings to `0.0.0.0`, never run them on a shared or internet-facing host, and never use their images or credentials for anything else.
- With Docker's `--network host`, or from another container on the same Docker network, they are reachable without going through those bindings.
- Issues in the demo servers are out of scope for security reports (see [SECURITY.md](../SECURITY.md#scope)).

---

## Known limitations

Things that work as described above but that a careful reviewer should know about. Apart from redirects (L2), none of them sends data to a host you didn't configure.

| # | Limitation | Impact | Workaround |
|---|---|---|---|
| L1 | `--env` values and `--key` appear in process command lines (mcpload's and the engine's `-e KEY=VALUE`). | Other local users can read tokens. | Export them as environment variables instead. |
| L2 | Redirects are followed: custom `MCP_HEADERS` go to any redirect host, `Authorization` survives a same-host `https`→`http` redirect, 307/308 re-send the body (including a `client_secret_post` token request). Applies to the engine, the readiness probe, baselines and uploads. | Credential exposure to a third host or over clear text, if the server or token endpoint redirects. | Use `https://` and URLs that don't redirect. |
| L3 | No size limit on successful MCP responses (JSON or SSE); one goroutine per server-to-client request. | A hostile server can exhaust the load generator's memory within the request timeout. | Keep `MCP_TIMEOUT` set (default 30 s); run against servers you trust. |
| L4 | Server-provided tool names are inserted into the PR comment and job summary without full Markdown escaping. | A server built from a malicious PR can add links, images or `@mentions` to the bot's comment. | Review comments on untrusted PRs; don't enable `comment-on-pr` where PR authors are untrusted. |
| L5 | Tool names and server error messages are printed to the terminal without removing control characters. | Terminal escape sequences from a hostile server reach your terminal or CI log. | Treat output from untrusted servers like any untrusted text. |
| L6 | Bare scenario names (and the default) are resolved in `./scenarios/` first; development builds also run `./k6` from the current folder. | Running mcpload in an untrusted folder runs that folder's code with your environment. | Pass `--scenario <path>`; use release builds; run from trusted folders. |
| L7 | The engine cache folder's ownership and permissions aren't checked, and there is a gap between the hash check and running the engine. | Someone who can write to the cache folder can run code as you. | Keep the default cache location; never use a shared `MCPLOAD_CACHE_DIR`. |
| L8 | The Docker image runs as root. | Reports in mounted folders are root-owned; the container has root's access to mounted files. | `docker run --user "$(id -u):$(id -g)"`. |
| L9 | Release assets are verified only against `checksums.txt` from the same release; nothing is signed. | A compromised release isn't detected. | Pin versions; build from source if you need more assurance. |
| L10 | Container names are passed to `docker` without a `--` separator. | A name starting with `-` is read as a `docker` option. | Only pass container names you chose. |
| L11 | The Action builds the latest k6 release (xk6's default) rather than a pinned version; the build cache key doesn't include the k6 version. | Builds at different times can use different k6 versions, and a new k6 release is picked up without review. | Use the Docker image or a release for a fixed engine. |
| L12 | `ELICITATION=1` answers `accept` to every elicitation request. | mcpload confirms whatever a tool asks to confirm. | Configure `ELICITATION='{"action":"decline"}'` for servers with destructive confirmations. |
| L13 | The scenario's start-up log line and the Action's echoed command line show `--url` unredacted. | A token in the URL reaches logs (GitHub masks it only if it came from `secrets`). | Don't put secrets in URLs. |
| L14 | The Action's PR comment update matches any comment that contains its hidden marker. | Someone can plant the marker so the bot edits their comment, or overwrite a comment that quotes it. | None needed in most repositories; use a distinctive `artifact-name`. |
