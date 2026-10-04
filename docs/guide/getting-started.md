# Install and try mcpload

[README](../../README.md) · [All docs](../README.md)

This is the long version of the [quickstart](../../README.md#quickstart): every install option, and a guided tour of the demo servers.

## Try it in 5 minutes

mcpload comes with small demo MCP servers, some healthy and some deliberately broken, so you can see both a pass and a fail without touching a real server.

**You need:** [Docker](https://docs.docker.com/get-docker/) (to run the demo servers). No Git or programming tools are needed.

**1. Start Docker.** Open Docker Desktop (Windows, Mac) or make sure the Docker service is running (Linux). `docker compose version` should print a version.

**2. Install mcpload.** One command downloads the right build for your computer, checks its SHA-256 checksum, and puts `mcpload` on your PATH. No admin rights needed.

Mac or Linux:

```bash
curl -fsSL https://raw.githubusercontent.com/atul121001/mcpload/main/install.sh | sh
```

Windows (PowerShell):

```powershell
irm https://raw.githubusercontent.com/atul121001/mcpload/main/install.ps1 | iex
```

npm (any OS with Node.js 18+). `npx mcpload ...` also works without installing; pin a version in a project with `npm install --save-dev mcpload`:

```bash
npm install -g mcpload
```

Homebrew (Mac or Linux):

```bash
brew install atul121001/tap/mcpload
```

Check it with `mcpload version`. mcpload is a single program with nothing else to install, and you can run it from any folder. Run the install command again to upgrade. On Windows, open a new terminal after installing so it picks up the new PATH.

<details>
<summary><b>Prefer to download by hand?</b></summary>

| Your computer | Download from [Releases](https://github.com/atul121001/mcpload/releases/latest) |
|---|---|
| Windows | `mcpload_<version>_windows_amd64.zip` |
| Mac with Apple silicon (M1 or newer) | `mcpload_<version>_darwin_arm64.tar.gz` |
| Mac with Intel | `mcpload_<version>_darwin_amd64.tar.gz` |
| Linux | `mcpload_<version>_linux_amd64.tar.gz` (or `linux_arm64`) |

Unpack it anywhere and keep the unpacked folder together. Run `./mcpload` (Windows: `.\mcpload.exe`) from that folder, or add the folder to your PATH. Check the download against `checksums.txt` from the same release (SHA-256).

> **Mac:** if macOS says the app "cannot be opened because the developer cannot be verified", run `xattr -dr com.apple.quarantine .` once in the unpacked folder. The install script does this for you.

</details>

**3. Start the demo servers.** They run only on your own machine (127.0.0.1).

```bash
mcpload demo up
```

The first time, this downloads the demo server images (a minute or two). It then starts 11 servers on `localhost:3001` to `3011`, waits until they answer, and lists each URL with what it demonstrates: a healthy server that passes, one that leaks memory, a load balancer that loses sessions, and so on. `mcpload demo status` shows them again.

**4. Test a healthy server.** One minute of agent traffic:

```bash
mcpload run --url http://localhost:3001/mcp --duration 1m --html report.html
```

With no `--scenario`, mcpload runs the standard agent-session test.

You should see a **PASS**. Open `report.html` in your browser to see per-tool timings.

**5. Now test a broken one.** This server sits behind a load balancer that forgets which agent belongs to which server:

```bash
mcpload run --url http://localhost:3004/mcp --scenario lb-check --html lb.html
```

You should see a **FAIL** with `session_not_found`. That's the tool catching a real class of bug.

> **On Windows,** the same commands work in PowerShell once mcpload is installed. [docs/QUICKSTART.md](../QUICKSTART.md) has PowerShell versions of everything.

<details>
<summary><b>Prefer to build from source?</b></summary>

You need [Go 1.26+](https://go.dev/dl/). From the repo folder, this builds the same single `mcpload` program as a release (mcpload's load engine is k6 with the MCP extension in [`xk6-mcpload/`](../../xk6-mcpload/README.md), built into the binary):

```bash
go install go.k6.io/xk6@latest
mkdir -p cmd/mcpload/internal/engine/bin
xk6 build --with github.com/atul121001/mcpload/xk6-mcpload=./xk6-mcpload --output cmd/mcpload/internal/engine/bin/engine
cp -r scenarios cmd/mcpload/internal/engine/bin/scenarios
(cd cmd/mcpload && go build -tags embedengine -o ../../mcpload .)
```

</details>

<details>
<summary><b>Working on mcpload itself? Run the demo servers from source</b></summary>

`mcpload demo up` runs published images. To change the demo servers, clone the repo and build them locally (you need Git and Docker):

```bash
git clone https://github.com/atul121001/mcpload.git
cd mcpload
docker compose -f demo-servers/docker-compose.yml up -d --build
```

They use the same ports and container names as `mcpload demo up`, so run one or the other. Stop them with `docker compose -f demo-servers/docker-compose.yml down`. See [demo-servers/README.md](../../demo-servers/README.md).

</details>

When you're done: `mcpload demo down`
