// Package demo holds the demo MCP servers that `mcpload demo` runs: a compose
// file embedded in the binary that uses the published images, and the list of
// targets with what each one demonstrates.
package demo

import (
	_ "embed"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

//go:embed compose.yml
var composeYAML string

const (
	// DefaultImagePrefix is where the release workflow publishes the demo
	// images: <prefix>-ts, -py, -go, -oauth and -nginx.
	DefaultImagePrefix = "ghcr.io/atul121001/mcpload-demo"
	// DefaultProject keeps container names as mcpload-demo-<service>-1, the
	// same as demo-servers/docker-compose.yml (docs use those names).
	DefaultProject = "mcpload-demo"
	// DefaultBasePort is the host port of the first target (ts-healthy).
	DefaultBasePort = 3001

	prefixVar = "${MCPLOAD_DEMO_IMAGE_PREFIX:-" + DefaultImagePrefix + "}"
	tagVar    = "${MCPLOAD_DEMO_TAG:-latest}"
)

// Options select the images and host ports of the rendered compose file.
type Options struct {
	ImagePrefix string // default DefaultImagePrefix
	Tag         string // default "latest"
	BasePort    int    // default DefaultBasePort; the other targets follow it
}

var hostPortRe = regexp.MustCompile(`"127\.0\.0\.1:(\d+):`)

// Compose returns the embedded compose file with the image prefix, tag and
// host ports filled in.
func Compose(o Options) (string, error) {
	if o.ImagePrefix == "" {
		o.ImagePrefix = DefaultImagePrefix
	}
	if o.Tag == "" {
		o.Tag = "latest"
	}
	if o.BasePort == 0 {
		o.BasePort = DefaultBasePort
	}
	last := o.BasePort + Targets[len(Targets)-1].Offset
	if o.BasePort < 1 || last > 65535 {
		return "", fmt.Errorf("base port %d: the demo needs ports %d-%d, which must be within 1-65535", o.BasePort, o.BasePort, last)
	}
	s := strings.ReplaceAll(composeYAML, "\r\n", "\n")
	s = strings.ReplaceAll(s, prefixVar, o.ImagePrefix)
	s = strings.ReplaceAll(s, tagVar, o.Tag)
	shift := o.BasePort - DefaultBasePort
	s = hostPortRe.ReplaceAllStringFunc(s, func(m string) string {
		p, _ := strconv.Atoi(hostPortRe.FindStringSubmatch(m)[1])
		return fmt.Sprintf(`"127.0.0.1:%d:`, p+shift)
	})
	return s, nil
}

// ImageTag maps an mcpload version to the demo image tag published with it:
// "v0.3.0" -> "0.3.0". Development builds use "latest".
func ImageTag(version string) string {
	v := strings.TrimPrefix(version, "v")
	if !releaseRe.MatchString(v) || strings.Contains(v, "dev") {
		return "latest"
	}
	return v
}

var releaseRe = regexp.MustCompile(`^\d+\.\d+\.\d+(-[0-9A-Za-z.]+)?$`)

// Target is one demo endpoint published on the host.
type Target struct {
	Service string // compose service name
	Offset  int    // host port = base port + Offset
	Path    string // "/mcp" for MCP endpoints
	Shows   string // what it demonstrates
}

// Targets lists the host-facing demo endpoints, in port order.
var Targets = []Target{
	{"ts-healthy", 0, "/mcp", "healthy server (TypeScript SDK): PASS"},
	{"ts-leaky", 1, "/mcp", "leaks ~1 MB per session: memory_leak FAIL (soak + docker sampler)"},
	{"py-healthy", 2, "/mcp", "healthy stateless server (Python SDK): PASS"},
	{"lb-stateful", 3, "/mcp", "load balancer without sticky sessions: session_not_found FAIL (lb-check)"},
	{"stateless-2026", 4, "/mcp", "stateless 2026-07-28 servers behind a load balancer: PASS (lb-check)"},
	{"mock-oauth", 5, "/token", "OAuth token server for ts-oauth (client mcpload:secret, 30 s tokens)"},
	{"ts-oauth", 6, "/mcp", "needs a Bearer token: token refresh under load (oauth-refresh)"},
	{"ts-pooled", 7, "/mcp", "tools share 2 slots: tool_isolation FAIL (isolation)"},
	{"skew", 8, "/mcp", "rolling deploy, mismatches fail fast: version_skew WARN (version-skew)"},
	{"skew-hang", 9, "/mcp", "rolling deploy, mismatches hang: version_skew FAIL (version-skew)"},
	{"ts-ignore-cancel", 10, "/mcp", "ignores cancellation: cancellation WARN/FAIL (CANCEL_RATE)"},
}

// Port is the target's host port for the given base port.
func (t Target) Port(base int) int { return base + t.Offset }

// URL is the target's endpoint on the host.
func (t Target) URL(base int) string {
	return fmt.Sprintf("http://localhost:%d%s", t.Port(base), t.Path)
}

// HealthURL is the target's /healthz on the host (127.0.0.1: the ports are
// bound there only).
func (t Target) HealthURL(base int) string {
	return fmt.Sprintf("http://127.0.0.1:%d/healthz", t.Port(base))
}
