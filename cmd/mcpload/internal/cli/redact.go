package cli

import (
	"net/url"
	"strings"
)

// redacted replaces a credential in a URL shown in logs or stored in report.json.
const redacted = "REDACTED"

// sensitiveParamWords mark a query parameter as a credential when its name,
// lowercased and without '-' and '_', contains one of them (api_key,
// access_token, client_secret, X-Amz-Signature, X-Amz-Credential, ...).
var sensitiveParamWords = []string{"token", "secret", "passw", "signature", "credential", "apikey", "accesskey", "auth"}

// sensitiveParamNames are short names that are credentials on their own
// (?key=..., Azure SAS ?sig=..., OAuth ?code=...).
var sensitiveParamNames = map[string]bool{"key": true, "sig": true, "code": true, "pwd": true}

func sensitiveParam(name string) bool {
	n := strings.ToLower(strings.NewReplacer("-", "", "_", "").Replace(name))
	if sensitiveParamNames[n] {
		return true
	}
	for _, w := range sensitiveParamWords {
		if strings.Contains(n, w) {
			return true
		}
	}
	return false
}

// redactURL returns s with credentials replaced by REDACTED: the password of
// user:password@ and the values of query parameters whose names look like a
// credential (see sensitiveParam). The rest of the URL, including the order
// and encoding of the other query parameters, is kept as given. A value that
// is not an absolute http(s) URL (e.g. a file path) is returned unchanged.
//
// report.json is uploaded, attached to CI runs and summarised in PR comments,
// and GitHub masks secrets only in job logs, so a token in --url must not be
// copied into it.
func redactURL(s string) string {
	u, err := url.Parse(s)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return s
	}
	changed := false
	if u.User != nil {
		if _, ok := u.User.Password(); ok {
			u.User = url.UserPassword(u.User.Username(), redacted)
			changed = true
		}
	}
	if u.RawQuery != "" {
		parts := strings.Split(u.RawQuery, "&")
		for i, p := range parts {
			name, _, hasValue := strings.Cut(p, "=")
			if dec, err := url.QueryUnescape(name); err == nil {
				name = dec
			}
			if hasValue && sensitiveParam(name) {
				k, _, _ := strings.Cut(p, "=")
				parts[i] = k + "=" + redacted
				changed = true
			}
		}
		u.RawQuery = strings.Join(parts, "&")
	}
	if !changed {
		return s
	}
	return u.String()
}
