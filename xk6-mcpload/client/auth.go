package client

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Auth produces the Authorization header value for a request.
type Auth interface {
	// Authorization returns the full header value (e.g. "Bearer abc").
	Authorization(ctx context.Context, hc *http.Client, obs Observer) (string, error)
	// Unauthorized is called when the server answered 401 to a request that
	// carried the given header value.
	Unauthorized(headerValue string)
}

// BearerAuth sends a static bearer token.
type BearerAuth struct{ Token string }

// Authorization implements Auth.
func (b BearerAuth) Authorization(context.Context, *http.Client, Observer) (string, error) {
	return "Bearer " + b.Token, nil
}

// Unauthorized implements Auth.
func (BearerAuth) Unauthorized(string) {}

// OAuthConfig configures an OAuth 2.0 client_credentials token source.
type OAuthConfig struct {
	TokenURL     string
	ClientID     string
	ClientSecret string
	Scope        string
	Audience     string
	// AuthStyle is "basic" (client_secret_basic, default) or "post"
	// (client_secret_post).
	AuthStyle string
	// FailureBackoff is how long a failed token fetch is cached: callers
	// within the window get the cached error without contacting the token
	// endpoint. 0 means DefaultFailureBackoff; a negative value disables the
	// negative cache.
	FailureBackoff time.Duration
}

// DefaultFailureBackoff is the negative-cache window for failed token
// fetches when OAuthConfig.FailureBackoff is 0.
const DefaultFailureBackoff = time.Second

func (c OAuthConfig) failureBackoff() time.Duration {
	switch {
	case c.FailureBackoff == 0:
		return DefaultFailureBackoff
	case c.FailureBackoff < 0:
		return 0
	}
	return c.FailureBackoff
}

func (c OAuthConfig) key() string {
	h := sha256.Sum256([]byte(strings.Join([]string{
		c.TokenURL, c.ClientID, c.ClientSecret, c.Scope, c.Audience, c.AuthStyle,
		c.failureBackoff().String(),
	}, "\x00")))
	return hex.EncodeToString(h[:])
}

// TokenSource caches a client_credentials token and refreshes it shortly
// before expiry. Concurrent callers share a single in-flight fetch
// (single-flight), so many VUs do not stampede the token endpoint. A failed
// fetch is cached for OAuthConfig.FailureBackoff (negative cache), so a bad
// credential or an unreachable IdP costs about one token request per window
// instead of one per connect.
type TokenSource struct {
	cfg OAuthConfig
	now func() time.Time

	mu        sync.Mutex
	token     string
	expiresAt time.Time
	refreshAt time.Time
	inflight  *tokenCall
	// Negative cache: the last fetch error and when it stops being served.
	failErr   *Error
	failUntil time.Time
}

type tokenCall struct {
	done  chan struct{}
	token string
	err   error
}

// NewTokenSource returns an unshared token source.
func NewTokenSource(cfg OAuthConfig) *TokenSource {
	return &TokenSource{cfg: cfg, now: time.Now}
}

var sharedSources sync.Map // key -> *TokenSource

// SharedTokenSource returns a process-wide token source for cfg, so that all
// VUs using the same credentials share one cached token.
func SharedTokenSource(cfg OAuthConfig) *TokenSource {
	k := cfg.key()
	if ts, ok := sharedSources.Load(k); ok {
		return ts.(*TokenSource)
	}
	ts, _ := sharedSources.LoadOrStore(k, NewTokenSource(cfg))
	return ts.(*TokenSource)
}

// Authorization implements Auth.
func (ts *TokenSource) Authorization(ctx context.Context, hc *http.Client, obs Observer) (string, error) {
	tok, err := ts.Token(ctx, hc, obs)
	if err != nil {
		return "", err
	}
	return "Bearer " + tok, nil
}

// Unauthorized implements Auth: a rejected token is dropped so the next
// request fetches a new one.
func (ts *TokenSource) Unauthorized(headerValue string) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	if ts.token != "" && headerValue == "Bearer "+ts.token {
		ts.token = ""
		ts.expiresAt = time.Time{}
		ts.refreshAt = time.Time{}
	}
}

// Token returns a valid access token, fetching one if needed.
func (ts *TokenSource) Token(ctx context.Context, hc *http.Client, obs Observer) (string, error) {
	if obs == nil {
		obs = NopObserver{}
	}
	ts.mu.Lock()
	now := ts.now()
	valid := ts.token != "" && now.Before(ts.expiresAt)
	if valid && now.Before(ts.refreshAt) {
		tok := ts.token
		ts.mu.Unlock()
		return tok, nil
	}
	if ts.failErr != nil && now.Before(ts.failUntil) {
		// Within the backoff window after a failed fetch: don't contact the
		// token endpoint. A still-valid token (failed soft refresh) is used.
		if valid {
			tok := ts.token
			ts.mu.Unlock()
			return tok, nil
		}
		ferr := ts.failErr
		ts.mu.Unlock()
		return "", ferr
	}
	if c := ts.inflight; c != nil {
		if valid { // soft refresh already running; keep using the current token
			tok := ts.token
			ts.mu.Unlock()
			return tok, nil
		}
		ts.mu.Unlock()
		select {
		case <-c.done:
			return c.token, c.err
		case <-ctx.Done():
			return "", &Error{Type: ErrAuth, Message: "waiting for token: " + ctx.Err().Error()}
		}
	}
	c := &tokenCall{done: make(chan struct{})}
	ts.inflight = c
	oldToken := ts.token
	ts.mu.Unlock()

	tok, lifetime, err := ts.fetch(ctx, hc, obs)

	ts.mu.Lock()
	ts.inflight = nil
	if err != nil {
		// Negative cache, unless the failure came from this caller's own
		// context ending (an iteration being cut off is not an IdP failure).
		if win := ts.cfg.failureBackoff(); win > 0 && ctx.Err() == nil {
			ts.failErr = cachedError(err)
			ts.failUntil = ts.now().Add(win)
		}
	} else {
		ts.failErr, ts.failUntil = nil, time.Time{}
		now = ts.now()
		ts.token = tok
		ts.expiresAt = now.Add(lifetime)
		skew := lifetime / 5
		if skew > time.Minute {
			skew = time.Minute
		}
		ts.refreshAt = ts.expiresAt.Add(-skew)
	}
	ts.mu.Unlock()

	c.token, c.err = tok, err
	close(c.done)
	if err != nil && valid {
		// A soft refresh failed but the old token is still valid.
		return oldToken, nil
	}
	return tok, err
}

func (ts *TokenSource) fetch(ctx context.Context, hc *http.Client, obs Observer) (string, time.Duration, error) {
	form := url.Values{"grant_type": {"client_credentials"}}
	if ts.cfg.Scope != "" {
		form.Set("scope", ts.cfg.Scope)
	}
	if ts.cfg.Audience != "" {
		form.Set("audience", ts.cfg.Audience)
	}
	basic := !strings.EqualFold(ts.cfg.AuthStyle, "post")
	if !basic {
		form.Set("client_id", ts.cfg.ClientID)
		form.Set("client_secret", ts.cfg.ClientSecret)
	}
	start := time.Now()
	stats := TokenStats{Start: start}
	fail := func(status int, msg string) (string, time.Duration, error) {
		stats.Status = status
		stats.ErrorType = ErrAuth
		stats.Duration = time.Since(start)
		obs.OnTokenFetch(stats)
		return "", 0, &Error{Type: ErrAuth, HTTPStatus: status, Message: msg}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, ts.cfg.TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return fail(0, "building token request: "+err.Error())
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	if basic {
		req.SetBasicAuth(url.QueryEscape(ts.cfg.ClientID), url.QueryEscape(ts.cfg.ClientSecret))
	}
	resp, err := hc.Do(req)
	if err != nil {
		return fail(0, "token request: "+err.Error())
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fail(resp.StatusCode, "reading token response: "+err.Error())
	}
	if resp.StatusCode/100 != 2 {
		return fail(resp.StatusCode, fmt.Sprintf("token endpoint returned %d: %s", resp.StatusCode, truncate(body, 200)))
	}
	var tr struct {
		AccessToken string      `json:"access_token"`
		ExpiresIn   json.Number `json:"expires_in"`
		Error       string      `json:"error"`
		ErrorDesc   string      `json:"error_description"`
	}
	if err := json.Unmarshal(body, &tr); err != nil {
		return fail(resp.StatusCode, "decoding token response: "+err.Error())
	}
	if tr.AccessToken == "" {
		return fail(resp.StatusCode, "token response has no access_token: "+tr.Error+" "+tr.ErrorDesc)
	}
	lifetime := time.Hour
	if tr.ExpiresIn != "" {
		if secs, err := tr.ExpiresIn.Float64(); err == nil && secs > 0 {
			lifetime = time.Duration(secs * float64(time.Second))
		}
	}
	stats.Status = resp.StatusCode
	stats.Duration = time.Since(start)
	obs.OnTokenFetch(stats)
	return tr.AccessToken, lifetime, nil
}

// cachedError copies a fetch error for serving from the negative cache,
// noting that the token endpoint was not contacted again.
func cachedError(err error) *Error {
	c := *AsError(err)
	c.Message += " (cached token endpoint failure; not retried until the failureBackoff window ends)"
	return &c
}

func truncate(b []byte, n int) string {
	if len(b) > n {
		return string(b[:n]) + "..."
	}
	return string(b)
}

// Config returns the source's configuration.
func (ts *TokenSource) Config() OAuthConfig { return ts.cfg }
