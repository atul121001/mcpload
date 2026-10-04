# mcpload (npm CLI)

> npm wrapper for [mcpload](https://github.com/atul121001/mcpload) — MCP load & soak testing for AI agents

This package is a thin wrapper that downloads the mcpload Go binary from [GitHub releases](https://github.com/atul121001/mcpload/releases), verifies its SHA-256 checksum, and runs it. All the heavy lifting is done by the Go binary.

## Install

```bash
npm install -g mcpload
# or use with npx (no global install needed):
npx mcpload --help
```

## Usage

```bash
# Run a load test
mcpload run --url http://localhost:3001/mcp

# Use with npx
npx mcpload run --url http://localhost:3001/mcp

# Capacity analysis
mcpload capacity --url http://localhost:3001/mcp --from 5 --to 50

# Compare two reports
mcpload compare report1.json report2.json

# Demo servers
mcpload demo up
mcpload run --url http://localhost:3001/mcp
mcpload demo down
```

## Related

- [mcpload](https://github.com/atul121001/mcpload) — The Go binary and full documentation