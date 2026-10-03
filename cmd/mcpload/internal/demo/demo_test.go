package demo

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// sourceCompose is the development compose file the embedded one mirrors.
var sourceCompose = filepath.Join("..", "..", "..", "..", "demo-servers", "docker-compose.yml")

func TestImageTag(t *testing.T) {
	for v, want := range map[string]string{
		"v0.3.0":           "0.3.0",
		"0.4.1":            "0.4.1",
		"v1.0.0-rc.1":      "1.0.0-rc.1",
		"0.1.0-dev":        "latest",
		"":                 "latest",
		"v0.3.0-dirty+abc": "latest",
		"main":             "latest",
	} {
		if got := ImageTag(v); got != want {
			t.Errorf("ImageTag(%q) = %q, want %q", v, got, want)
		}
	}
}

func TestComposeDefaults(t *testing.T) {
	y, err := Compose(Options{Tag: "0.3.0"})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{
		"image: ghcr.io/atul121001/mcpload-demo-ts:0.3.0",
		"image: ghcr.io/atul121001/mcpload-demo-py:0.3.0",
		"image: ghcr.io/atul121001/mcpload-demo-go:0.3.0",
		"image: ghcr.io/atul121001/mcpload-demo-oauth:0.3.0",
		"image: ghcr.io/atul121001/mcpload-demo-nginx:0.3.0",
		`"127.0.0.1:3001:3000"`,
		`"127.0.0.1:3011:3000"`,
	} {
		if !strings.Contains(y, s) {
			t.Errorf("rendered compose lacks %q", s)
		}
	}
	if strings.Contains(y, "${") || strings.Contains(y, "\r") {
		t.Error("rendered compose still has ${...} or CR")
	}
}

func TestComposeOverrides(t *testing.T) {
	y, err := Compose(Options{ImagePrefix: "mcpload-demo-local/demo", Tag: "dev", BasePort: 13001})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(y, "image: mcpload-demo-local/demo-ts:dev") {
		t.Error("image prefix/tag not applied")
	}
	ports := regexp.MustCompile(`"127\.0\.0\.1:(\d+):\d+"`).FindAllStringSubmatch(y, -1)
	if len(ports) != len(Targets) {
		t.Fatalf("%d published ports, want %d", len(ports), len(Targets))
	}
	for i, m := range ports {
		if want := 13001 + i; m[1] != strconv.Itoa(want) {
			t.Errorf("port %d = %s, want %d", i, m[1], want)
		}
	}
	if _, err := Compose(Options{BasePort: 65530}); err == nil {
		t.Error("base port 65530 should be rejected (needs 11 ports)")
	}
}

// TestTargetsMatchCompose: every target is a service publishing the port the
// table says, in the embedded file.
func TestTargetsMatchCompose(t *testing.T) {
	published := servicePorts(t, composeYAML)
	if len(published) != len(Targets) {
		t.Errorf("compose publishes %d services, Targets lists %d", len(published), len(Targets))
	}
	for _, tg := range Targets {
		if got := published[tg.Service]; got != strconv.Itoa(DefaultBasePort+tg.Offset) {
			t.Errorf("%s: compose publishes host port %q, Targets says %d", tg.Service, got, DefaultBasePort+tg.Offset)
		}
	}
}

// TestEmbeddedComposeServicesInSync is a docker-free check that the embedded
// compose file has the same services and host ports as demo-servers/.
func TestEmbeddedComposeServicesInSync(t *testing.T) {
	src, err := os.ReadFile(sourceCompose)
	if err != nil {
		t.Fatal(err)
	}
	s := strings.ReplaceAll(string(src), "\r\n", "\n")
	if got, want := serviceNames(t, composeYAML), serviceNames(t, s); !reflect.DeepEqual(got, want) {
		t.Errorf("services differ:\nembedded %v\nsource   %v", got, want)
	}
	if got, want := servicePorts(t, composeYAML), servicePorts(t, s); !reflect.DeepEqual(got, want) {
		t.Errorf("published ports differ:\nembedded %v\nsource   %v", got, want)
	}
}

var serviceRe = regexp.MustCompile(`(?m)^  ([a-z0-9-]+):\s*$`)

func servicesSection(t *testing.T, y string) string {
	t.Helper()
	y = strings.ReplaceAll(y, "\r\n", "\n")
	i := strings.Index(y, "\nservices:\n")
	if i < 0 {
		t.Fatal("no services: section")
	}
	return y[i:]
}

func serviceNames(t *testing.T, y string) []string {
	var names []string
	for _, m := range serviceRe.FindAllStringSubmatch(servicesSection(t, y), -1) {
		names = append(names, m[1])
	}
	sort.Strings(names)
	return names
}

// servicePorts maps service -> host port for services that publish one.
func servicePorts(t *testing.T, y string) map[string]string {
	sec := servicesSection(t, y)
	idx := serviceRe.FindAllStringSubmatchIndex(sec, -1)
	portRe := regexp.MustCompile(`ports: \["127\.0\.0\.1:(\d+):\d+"\]`)
	out := map[string]string{}
	for i, m := range idx {
		end := len(sec)
		if i+1 < len(idx) {
			end = idx[i+1][0]
		}
		if p := portRe.FindStringSubmatch(sec[m[1]:end]); p != nil {
			out[sec[m[2]:m[3]]] = p[1]
		}
	}
	return out
}

// TestEmbeddedComposeInSync compares the fully resolved embedded compose file
// with demo-servers/docker-compose.yml using `docker compose config` (so
// anchors and merges are resolved the way compose does): same services,
// ports, env, memory limits, restart policy, hostnames and dependencies, and
// the matching published image for each source image. Skipped without docker.
func TestEmbeddedComposeInSync(t *testing.T) {
	if err := exec.Command("docker", "compose", "version").Run(); err != nil {
		t.Skip("docker compose not available:", err)
	}
	src := composeConfig(t, nil, "--file", sourceCompose)
	y, err := Compose(Options{Tag: "test"})
	if err != nil {
		t.Fatal(err)
	}
	emb := composeConfig(t, strings.NewReader(y), "--file", "-")

	if got, want := keys(emb), keys(src); !reflect.DeepEqual(got, want) {
		t.Fatalf("services differ:\nembedded %v\nsource   %v", got, want)
	}
	imageKind := map[string]string{
		"mcpload-demo/ts-server": "ts", "mcpload-demo/go-server": "go", "mcpload-demo/py-server": "py",
		"mcpload-demo/mock-oauth": "oauth", "nginx": "nginx",
	}
	for name, s := range src {
		e := emb[name]
		for _, field := range []string{"ports", "environment", "mem_limit", "restart", "hostname", "depends_on"} {
			if !reflect.DeepEqual(e[field], s[field]) {
				t.Errorf("%s.%s: embedded %s, source %s", name, field, js(e[field]), js(s[field]))
			}
		}
		srcImage, _ := s["image"].(string)
		repo := srcImage[:strings.LastIndex(srcImage, ":")]
		kind, ok := imageKind[repo]
		if !ok {
			t.Errorf("%s: source image %q has no published counterpart; add it to release.yml and this test", name, srcImage)
			continue
		}
		if want := DefaultImagePrefix + "-" + kind + ":test"; e["image"] != want {
			t.Errorf("%s: embedded image %v, want %s", name, e["image"], want)
		}
		if kind == "nginx" {
			// The source mounts ./nginx/<conf>; the published image bakes it in.
			vols, _ := json.Marshal(s["volumes"])
			conf := regexp.MustCompile(`nginx[/\\\\]+([a-z0-9-]+\.conf)`).FindStringSubmatch(string(vols))
			if conf == nil || !strings.Contains(js(e["command"]), "/etc/nginx/mcpload/"+conf[1]) {
				t.Errorf("%s: embedded command %s does not load the mounted config %s", name, js(e["command"]), vols)
			}
		}
	}
}

func composeConfig(t *testing.T, stdin *strings.Reader, args ...string) map[string]map[string]any {
	t.Helper()
	cmd := exec.Command("docker", append([]string{"compose"}, append(args, "config", "--format", "json")...)...)
	if stdin != nil {
		cmd.Stdin = stdin
	}
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		t.Fatalf("docker compose config: %v\n%s", err, errb.String())
	}
	var cfg struct {
		Services map[string]map[string]any `json:"services"`
	}
	if err := json.Unmarshal(out.Bytes(), &cfg); err != nil {
		t.Fatal(err)
	}
	return cfg.Services
}

func keys(m map[string]map[string]any) []string {
	var k []string
	for n := range m {
		k = append(k, n)
	}
	sort.Strings(k)
	return k
}

func js(v any) string { b, _ := json.Marshal(v); return string(b) }
