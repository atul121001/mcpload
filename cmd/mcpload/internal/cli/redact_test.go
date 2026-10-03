package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/atul121001/mcpload/cmd/mcpload/internal/report"
)

func TestRedactURL(t *testing.T) {
	cases := []struct{ in, want string }{
		// Nothing to redact: returned byte for byte.
		{"http://localhost:3001/mcp", "http://localhost:3001/mcp"},
		{"https://example.com/mcp?tenant=a&b=%2F", "https://example.com/mcp?tenant=a&b=%2F"},
		{"https://user@example.com/mcp", "https://user@example.com/mcp"},
		{"report.json", "report.json"},
		{`C:\reports\main.json`, `C:\reports\main.json`},
		{"", ""},
		// Credentials.
		{"https://user:s3cret@example.com/mcp", "https://user:REDACTED@example.com/mcp"},
		{"https://example.com/mcp?api_key=s3cret&tenant=a", "https://example.com/mcp?api_key=REDACTED&tenant=a"},
		{"https://example.com/mcp?tenant=a&access_token=s3cret", "https://example.com/mcp?tenant=a&access_token=REDACTED"},
		{"https://example.com/mcp?key=s3cret", "https://example.com/mcp?key=REDACTED"},
		{"https://example.com/mcp?Api-Key=s3cret", "https://example.com/mcp?Api-Key=REDACTED"},
		{"https://example.com/mcp?client%5Fsecret=s3cret", "https://example.com/mcp?client%5Fsecret=REDACTED"},
		{"https://b.s3.amazonaws.com/main.json?X-Amz-Credential=AKIA%2F1&X-Amz-Signature=abc&X-Amz-Expires=60",
			"https://b.s3.amazonaws.com/main.json?X-Amz-Credential=REDACTED&X-Amz-Signature=REDACTED&X-Amz-Expires=60"},
		{"https://a.blob.core.windows.net/r.json?sv=2024&sig=abc%3D", "https://a.blob.core.windows.net/r.json?sv=2024&sig=REDACTED"},
		// A parameter without a value has nothing to hide.
		{"https://example.com/mcp?token", "https://example.com/mcp?token"},
	}
	for _, tc := range cases {
		if got := redactURL(tc.in); got != tc.want {
			t.Errorf("redactURL(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// A baseline read from a URL with a token in its query string is recorded in
// report.json (comparison.baseline.source) without the token.
func TestCompareRedactsBaselineSource(t *testing.T) {
	healthy := filepath.Join(examples, "healthy.json")
	b, err := os.ReadFile(healthy)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("token") != "s3cret" {
			http.Error(w, "no token", http.StatusUnauthorized)
			return
		}
		_, _ = w.Write(b)
	}))
	defer srv.Close()
	var out, errb bytes.Buffer
	code := Main([]string{"compare", srv.URL + "/main.json?token=s3cret", healthy, "--format", "json"}, &out, &errb)
	if code != ExitPass {
		t.Fatalf("exit %d, stderr %s", code, errb.String())
	}
	if strings.Contains(out.String(), "s3cret") {
		t.Fatalf("token leaked into the comparison: %s", out.String())
	}
	var c report.Comparison
	if err := json.Unmarshal(out.Bytes(), &c); err != nil {
		t.Fatal(err)
	}
	if want := srv.URL + "/main.json?token=REDACTED"; c.Baseline.Source != want {
		t.Errorf("source %q, want %q", c.Baseline.Source, want)
	}
}
