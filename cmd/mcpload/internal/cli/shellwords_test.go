package cli

import (
	"reflect"
	"strings"
	"testing"

	"github.com/atul121001/mcpload/cmd/mcpload/internal/report"
)

func TestSplitCommand(t *testing.T) {
	cases := []struct {
		in   string
		want []string
		err  string
	}{
		{in: "node server.mjs", want: []string{"node", "server.mjs"}},
		{in: "  node \t server.mjs\n --port 0  ", want: []string{"node", "server.mjs", "--port", "0"}},
		{in: `python -m "my server" --name 'a b'`, want: []string{"python", "-m", "my server", "--name", "a b"}},
		{in: `node "C:\Program Files\srv\index.js"`, want: []string{"node", `C:\Program Files\srv\index.js`}},
		{in: `C:\servers\mcp.exe --flag`, want: []string{`C:\servers\mcp.exe`, "--flag"}},
		{in: `node my\ server.js`, want: []string{"node", "my server.js"}},
		{in: `echo "say \"hi\"" 'it''s'`, want: []string{"echo", `say "hi"`, "its"}},
		{in: `echo 'no \escapes "here"'`, want: []string{"echo", `no \escapes "here"`}},
		{in: `echo "a\\b" "c\d"`, want: []string{"echo", `a\b`, `c\d`}},
		{in: `echo \\host\share`, want: []string{"echo", `\host\share`}},
		{in: `echo '\\host\share'`, want: []string{"echo", `\\host\share`}},
		{in: `prog "" ''`, want: []string{"prog", "", ""}},
		{in: `prog a"b c"d`, want: []string{"prog", "ab cd"}},
		{in: `prog $HOME *.js | tee x > y; rm -rf /`, want: []string{"prog", "$HOME", "*.js", "|", "tee", "x", ">", "y;", "rm", "-rf", "/"}},
		{in: `node "unterminated`, err: `unterminated "`},
		{in: `node 'unterminated`, err: "unterminated '"},
		{in: "   ", err: "no program"},
		{in: `"" arg`, err: "no program"},
	}
	for _, c := range cases {
		got, err := splitCommand(c.in)
		if c.err != "" {
			if err == nil || !strings.Contains(err.Error(), c.err) {
				t.Errorf("%q: err = %v, want %q", c.in, err, c.err)
			}
			continue
		}
		if err != nil || !reflect.DeepEqual(got, c.want) {
			t.Errorf("%q: got %q, %v; want %q", c.in, got, err, c.want)
		}
	}
}

// TestQuoteCommandRoundTrip: the display form splits back to the same words.
func TestQuoteCommandRoundTrip(t *testing.T) {
	for _, argv := range [][]string{
		{"node", "server.mjs"},
		{"node", "my server.js", "", `C:\x\y`, `it's`, `say "hi"`, `a\"b`},
	} {
		s := report.QuoteCommand(argv)
		back, err := splitCommand(s)
		if err != nil || !reflect.DeepEqual(back, argv) {
			t.Errorf("%q -> %q -> %q (%v)", argv, s, back, err)
		}
	}
}

func TestRedactArgs(t *testing.T) {
	in := []string{"node", "srv.js", "--token=abc", "--api-key", "xyz", "--port", "3000", "--url=https://u:pw@h/x?access_token=t&a=1", "https://h/?key=k", "-v"}
	got := redactArgs(in)
	want := []string{"node", "srv.js", "--token=REDACTED", "--api-key", "REDACTED", "--port", "3000", "--url=https://u:REDACTED@h/x?access_token=REDACTED&a=1", "https://h/?key=REDACTED", "-v"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got  %q\nwant %q", got, want)
	}
	if in[2] != "--token=abc" {
		t.Error("input modified")
	}
	// A sensitive flag followed by another flag has no value to redact.
	if got := redactArgs([]string{"p", "--secret", "--verbose"}); got[2] != "--verbose" {
		t.Errorf("got %q", got)
	}
}

func TestParseCommandEnv(t *testing.T) {
	m, err := parseCommandEnv([]string{"A=1", "B=x=y", "A=2", "EMPTY="})
	if err != nil || m["A"] != "2" || m["B"] != "x=y" || m["EMPTY"] != "" || len(m) != 3 {
		t.Fatalf("got %v %v", m, err)
	}
	for _, bad := range []string{"sk-secret-value", "=v", "A B=sk-secret-value"} {
		_, err := parseCommandEnv([]string{bad})
		if err == nil {
			t.Fatalf("%q accepted", bad)
		}
		if strings.Contains(err.Error(), "secret") {
			t.Errorf("error leaks the value: %v", err)
		}
	}
}
