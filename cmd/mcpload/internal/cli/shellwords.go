package cli

import (
	"errors"
	"fmt"
	"strings"
)

// splitCommand splits a --command value into program and arguments the way a
// POSIX shell splits words, without running a shell (no variables, globbing,
// pipes or redirection; those characters are kept literally):
//
//   - unquoted spaces, tabs and newlines separate words;
//   - '...' is literal (no escapes inside);
//   - "..." is literal except that \" and \\ stand for " and \;
//   - outside quotes, a backslash before a space, tab, quote or backslash
//     escapes it; any other backslash is kept, so Windows paths such as
//     C:\servers\mcp.exe need no doubling (write UNC paths \\host\share in
//     single quotes);
//   - empty quotes ("" or two single quotes) make an empty argument.
func splitCommand(s string) ([]string, error) {
	var (
		words   []string
		cur     strings.Builder
		inWord  bool // cur holds a word (possibly empty, from "")
		rs      = []rune(s)
		escaped = func(r rune) bool { return r == ' ' || r == '\t' || r == '\n' || r == '\'' || r == '"' || r == '\\' }
	)
	for i := 0; i < len(rs); i++ {
		r := rs[i]
		switch {
		case r == ' ' || r == '\t' || r == '\n' || r == '\r':
			if inWord {
				words = append(words, cur.String())
				cur.Reset()
				inWord = false
			}
		case r == '\'':
			inWord = true
			j := i + 1
			for j < len(rs) && rs[j] != '\'' {
				j++
			}
			if j == len(rs) {
				return nil, errors.New("unterminated ' quote")
			}
			cur.WriteString(string(rs[i+1 : j]))
			i = j
		case r == '"':
			inWord = true
			j := i + 1
			for ; j < len(rs) && rs[j] != '"'; j++ {
				if rs[j] == '\\' && j+1 < len(rs) && (rs[j+1] == '"' || rs[j+1] == '\\') {
					j++
				}
				cur.WriteRune(rs[j])
			}
			if j == len(rs) {
				return nil, errors.New(`unterminated " quote`)
			}
			i = j
		case r == '\\' && i+1 < len(rs) && escaped(rs[i+1]):
			inWord = true
			cur.WriteRune(rs[i+1])
			i++
		default:
			inWord = true
			cur.WriteRune(r)
		}
	}
	if inWord {
		words = append(words, cur.String())
	}
	if len(words) == 0 || words[0] == "" {
		return nil, errors.New("no program given")
	}
	return words, nil
}

// redactArgs returns a copy of argv with credential-looking argument values
// replaced by REDACTED, for report.json and logs: --token=x, --api-key x,
// URL credentials (see redactURL). The program itself is kept.
func redactArgs(argv []string) []string {
	out := append([]string(nil), argv...)
	for i := 1; i < len(out); i++ {
		a := out[i]
		if !strings.HasPrefix(a, "-") {
			out[i] = redactURL(a)
			continue
		}
		name, val, hasVal := strings.Cut(strings.TrimLeft(a, "-"), "=")
		if !sensitiveParam(name) {
			if hasVal {
				out[i] = a[:len(a)-len(val)] + redactURL(val)
			}
			continue
		}
		if hasVal {
			out[i] = a[:len(a)-len(val)] + redacted
		} else if i+1 < len(out) && !strings.HasPrefix(out[i+1], "-") {
			out[i+1] = redacted
			i++
		}
	}
	return out
}

// parseCommandEnv turns --command-env K=V values into a map (later wins).
func parseCommandEnv(kvs []string) (map[string]string, error) {
	m := map[string]string{}
	for i, kv := range kvs {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || k == "" || strings.ContainsAny(k, " \t") {
			// Never echo the argument: it may be a secret (the whole of it,
			// when the KEY= part was forgotten).
			return nil, fmt.Errorf("--command-env #%d must be KEY=VALUE (the value is not shown)", i+1)
		}
		m[k] = v
	}
	return m, nil
}
