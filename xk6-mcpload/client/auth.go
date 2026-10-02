package client

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
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
	// Timeout bounds one token fetch. 0 means the Options.Timeout of the
	// session whose request started the fetch, or DefaultTokenTimeout.
	Timeout time.Duration
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
		c.failureBackoff().String(), c.Timeout.String(),
	}, "\x00")))
	return hex.EncodeToString(h[:])
}

// TokenSource caches a client_credentials token and refreshes it shortly
// before expiry (in the background, while the old token is still served).
// Concurrent callers share a single in-flight fetch (single-flight), so many
// VUs do not stampede the token endpoint. A failed
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
//
// The fetch itself runs in a background goroutine on a context detached from
// the caller (bounded by the fetch timeout, see FetchTimeout), so a caller
// whose context ends does not fail the fetch for everyone else sharing it.
// Every caller, including the one that started the fetch, waits with its own
// ctx. When the cached token is still valid but inside the refresh window,
// the refresh is started in the background and the current token is returned
// immediately (soft refresh never blocks).
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
	c := ts.inflight
	if c == nil {
		c = &tokenCall{done: make(chan struct{})}
		ts.inflight = c
		go ts.runFetch(c, fetchTimeout(ctx, ts.cfg.Timeout), hc, obs)
	}
	if valid {
		// Soft refresh (started now or already running): keep using the
		// current token without waiting.
		tok := ts.token
		ts.mu.Unlock()
		return tok, nil
	}
	ts.mu.Unlock()
	select {
	case <-c.done:
		return c.token, c.err
	case <-ctx.Done():
		typ := ErrAuth
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			typ = ErrTimeout
		}
		return "", &Error{Type: typ, Message: "waiting for token: " + ctx.Err().Error()}
	}
}

// runFetch performs one token fetch on a detached context and publishes the
// outcome to the source and to everyone waiting on c.
func (ts *TokenSource) runFetch(c *tokenCall, timeout time.Duration, hc *http.Client, obs Observer) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	tok, lifetime, err := ts.fetch(ctx, hc, obs)
	cancel()

	ts.mu.Lock()
	ts.inflight = nil
	if err != nil {
		// The fetch runs on its own context, so any failure (including its
		// own timeout) is an IdP failure and is negatively cached.
		if win := ts.cfg.failureBackoff(); win > 0 {
			ts.failErr = cachedError(err)
			ts.failUntil = ts.now().Add(win)
		}
	} else {
		ts.failErr, ts.failUntil = nil, time.Time{}
		now := ts.now()
		ts.token = tok
		ts.expiresAt = now.Add(lifetime)
		skew := lifetime / 5
		if skew > time.Minute {
			skew = time.Minute
		}
		ts.refreshAt = ts.expiresAt.Add(-skew)
	}
	c.token, c.err = tok, err
	ts.mu.Unlock()
	close(c.done)
}

type fetchTimeoutKey struct{}

// WithFetchTimeout returns a context that tells a TokenSource how long a
// token fetch started by this caller may take, when OAuthConfig.Timeout is
// not set. Session uses it to apply Options.Timeout to token fetches.
func WithFetchTimeout(ctx context.Context, d time.Duration) context.Context {
	return context.WithValue(ctx, fetchTimeoutKey{}, d)
}

// DefaultTokenTimeout bounds a token fetch when neither OAuthConfig.Timeout
// nor the caller's Options.Timeout is set.
const DefaultTokenTimeout = 30 * time.Second

func fetchTimeout(ctx context.Context, cfg time.Duration) time.Duration {
	if cfg > 0 {
		return cfg
	}
	if d, ok := ctx.Value(fetchTimeoutKey{}).(time.Duration); ok && d > 0 {
		return d
	}
	return DefaultTokenTimeout
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
	// A fetch that ran out of its own time budget is a timeout, not an
	// authentication failure.
	failTimeout := func(ctx context.Context, err error, status int, prefix string) (string, time.Duration, error) {
		te := transportError(ctx, err)
		if te.Type != ErrTimeout {
			return fail(status, prefix+err.Error())
		}
		stats.Status = status
		stats.ErrorType = ErrTimeout
		stats.Duration = time.Since(start)
		obs.OnTokenFetch(stats)
		return "", 0, &Error{Type: ErrTimeout, HTTPStatus: status, Message: prefix + err.Error()}
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
		return failTimeout(ctx, err, 0, "token request: ")
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return failTimeout(ctx, err, resp.StatusCode, "reading token response: ")
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
