package client

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/http/httptrace"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// maxErrorBody bounds how much of a non-2xx body is read for classification.
const maxErrorBody = 64 << 10

// EncodeHeaderValue returns s unchanged when it is a safe ASCII header value,
// or wraps it as =?base64?<std-base64>?= when it contains non-ASCII or control
// characters or leading/trailing whitespace, or when it already looks like an
// encoded value (=?base64?...?=), so the server never mis-decodes a literal.
func EncodeHeaderValue(s string) string {
	if !needsBase64(s) {
		return s
	}
	return "=?base64?" + base64.StdEncoding.EncodeToString([]byte(s)) + "?="
}

func needsBase64(s string) bool {
	if s == "" {
		return false
	}
	if len(s) >= len("=?base64??=") && strings.EqualFold(s[:len("=?base64?")], "=?base64?") && strings.HasSuffix(s, "?=") {
		return true
	}
	if s[0] == ' ' || s[0] == '\t' || s[len(s)-1] == ' ' || s[len(s)-1] == '\t' {
		return true
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < 0x20 || c > 0x7e {
			return true
		}
	}
	return false
}

// exchange is one HTTP POST of a JSON-RPC message.
type exchange struct {
	method    string
	tool      string
	id        *int64 // nil for notifications
	params    any
	nameHdr   string // Mcp-Name value (stateless only)
	sessionID string
	protoHdr  string // Mcp-Protocol-Version value; "" to omit
	stateless bool
	// cancelAfter > 0: cancel the request when no response arrived within
	// it (see cancelInFlight).
	cancelAfter time.Duration
}

type exchangeResult struct {
	result    json.RawMessage
	sessionID string // Mcp-Session-Id response header
	servedBy  string // Options.ServedByHeader response header
	stats     RequestStats
	err       *Error
}

// post performs one JSON-RPC exchange. It never retries.
func (s *Session) post(ctx context.Context, ex exchange) exchangeResult {
	res := exchangeResult{stats: RequestStats{Method: ex.method, Tool: ex.tool, Protocol: s.protocolTag(ex)}}
	body, err := json.Marshal(rpcRequest{JSONRPC: "2.0", ID: ex.id, Method: ex.method, Params: ex.params})
	if err != nil {
		res.err = &Error{Type: ErrJSONRPC, Message: "encoding request: " + err.Error()}
		return res
	}

	authHdr, aerr := s.authorization(ctx)
	if aerr != nil {
		res.err = aerr
		res.stats.ErrorType = aerr.Type
		res.stats.Start = time.Now()
		res.stats.NotSent = true
		s.obs(ctx).OnRequest(res.stats)
		return res
	}

	// Server-to-client requests on the response stream are answered in
	// goroutines with the caller's context (each answer POST has its own
	// timeout); post returns only after every answer has finished (except
	// for a cancelled request, which stops waiting at once).
	parent := ctx
	var answers sync.WaitGroup
	onRequest := func(m *rpcMessage) {
		answers.Add(1)
		go func() {
			defer answers.Done()
			s.answer(parent, ex, m)
		}()
	}

	ctx, cancel := s.withTimeout(ctx)

	// GotFirstResponseByte runs on the transport's goroutine, which can still
	// be running when Do has already returned an error (timeout), so the
	// time is published atomically: nanoseconds since base, 0 = not seen.
	base := time.Now()
	var ttfb atomic.Int64
	trace := &httptrace.ClientTrace{GotFirstResponseByte: func() { ttfb.Store(int64(time.Since(base)) + 1) }}
	req, err := http.NewRequestWithContext(httptrace.WithClientTrace(ctx, trace), http.MethodPost, s.opts.URL, bytes.NewReader(body))
	if err != nil {
		cancel()
		res.err = &Error{Type: ErrHTTP, Message: "building request: " + err.Error()}
		return res
	}
	s.setCommonHeaders(req, authHdr)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if ex.sessionID != "" {
		req.Header.Set(HeaderSessionID, ex.sessionID)
	}
	if ex.protoHdr != "" {
		req.Header.Set(HeaderProtocolVersion, ex.protoHdr)
	}
	if ex.stateless {
		req.Header.Set(HeaderMethod, ex.method)
		if ex.nameHdr != "" {
			req.Header.Set(HeaderName, EncodeHeaderValue(ex.nameHdr))
		}
	}

	start := time.Now()
	res.stats.Start = start
	finish := func() {
		res.stats.Duration = time.Since(start)
		if v := ttfb.Load(); v != 0 {
			if d := time.Duration(v-1) - start.Sub(base); d > 0 {
				res.stats.TTFB = d
			}
		}
		if res.err != nil {
			res.stats.ErrorType = res.err.Type
			res.err.ServedBy = res.servedBy
		}
		s.obs(ctx).OnRequest(res.stats)
	}

	if ex.cancelAfter <= 0 {
		defer answers.Wait()
		s.settle(&res, s.roundTrip(ctx, cancel, req, ex, authHdr, onRequest, nil))
		finish()
		s.cancelOnTimeout(parent, ex, res.err)
		return res
	}

	// Cancellable call: the round trip runs in a goroutine so that the
	// caller can stop waiting when cancelAfter passes without a response.
	var early earlyHeaders
	done := make(chan wire, 1)
	go func() { done <- s.roundTrip(ctx, cancel, req, ex, authHdr, onRequest, &early) }()
	timer := time.NewTimer(ex.cancelAfter)
	var w wire
	select {
	case w = <-done:
		timer.Stop()
	case <-timer.C:
		select {
		case w = <-done: // the response won the race after all
		default:
			// The response headers may already be in (a slow SSE stream):
			// keep the status and the replica that answered.
			res.stats.Status, res.servedBy = early.get()
			res.err = &Error{Type: ErrCancelled, HTTPStatus: res.stats.Status,
				Message: fmt.Sprintf("cancelled by the client after %s", ex.cancelAfter)}
			finish()
			s.cancelInFlight(parent, cancel, ex, done, &answers)
			return res
		}
	}
	s.settle(&res, w)
	finish()
	answers.Wait()
	if obs, ok := s.obs(parent).(CancelObserver); ok {
		obs.OnCancel(CancelStats{Method: ex.method, Tool: ex.tool, Protocol: s.protocolTag(ex),
			Reason: CancelReasonClient, Outcome: CancelOutcomeCompleted, Start: time.Now()})
	}
	s.cancelOnTimeout(parent, ex, res.err)
	return res
}

// wire is the outcome of one HTTP round trip (roundTrip).
type wire struct {
	status    int
	sessionID string
	servedBy  string // Options.ServedByHeader response header
	streamed  bool
	stream    time.Duration
	msg       *rpcMessage // nil for a notification or on error
	err       *Error
}

// earlyHeaders publishes what the response headers said as soon as they
// arrive, while the round trip is still reading the body: a cancelled call
// reports them without waiting for the round trip to end.
type earlyHeaders struct {
	mu       sync.Mutex
	status   int
	servedBy string
}

func (e *earlyHeaders) set(status int, servedBy string) {
	e.mu.Lock()
	e.status, e.servedBy = status, servedBy
	e.mu.Unlock()
}

func (e *earlyHeaders) get() (int, string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.status, e.servedBy
}

// settle copies a round trip's outcome into res (without reporting it).
func (s *Session) settle(res *exchangeResult, w wire) {
	res.stats.Status = w.status
	res.sessionID = w.sessionID
	res.servedBy = w.servedBy
	res.stats.Streamed, res.stats.Stream = w.streamed, w.stream
	switch {
	case w.err != nil:
		res.err = w.err
	case w.msg == nil: // notification
	case w.msg.Error != nil:
		res.err = rpcToError(w.status, w.msg.Error)
	default:
		res.result = w.msg.Result
	}
}

// roundTrip sends req and reads the response to ex. It releases ctx (calls
// cancel) before returning, or after the background drain of an SSE stream.
// early, when set, receives the HTTP status and served-by header as soon as
// headers arrive.
func (s *Session) roundTrip(ctx context.Context, cancel context.CancelFunc, req *http.Request, ex exchange, authHdr string,
	onRequest func(*rpcMessage), early *earlyHeaders) (w wire) {
	cancelOwned := true
	defer func() {
		if cancelOwned {
			cancel()
		}
	}()
	resp, err := s.hc.Do(req)
	if err != nil {
		w.err = transportError(ctx, err)
		return w
	}
	headersAt := time.Now()
	w.status = resp.StatusCode
	w.sessionID = resp.Header.Get(HeaderSessionID)
	w.servedBy = resp.Header.Get(s.opts.ServedByHeader)
	if early != nil {
		early.set(w.status, w.servedBy)
	}

	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
		_ = resp.Body.Close()
		w.err = s.classifyStatus(resp, b, ex.sessionID != "", authHdr)
		return w
	}

	if ex.id == nil { // notification: 202 Accepted (or any 2xx) with no payload
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxErrorBody))
		_ = resp.Body.Close()
		return w
	}

	mt, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	wantID := strconv.FormatInt(*ex.id, 10)
	if mt == "text/event-stream" {
		w.streamed = true
		w.msg, w.err = readSSE(resp.Body, wantID, onRequest)
		w.stream = time.Since(headersAt)
		// Drain the rest of the stream in the background so the connection
		// can be reused, without holding up the caller or its timings.
		cancelOwned = false
		go drainAndClose(resp.Body, cancel)
	} else {
		b, rerr := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if rerr != nil {
			w.err = transportError(ctx, rerr)
		} else {
			w.msg, w.err = findResponse(b, wantID)
		}
	}
	if w.err != nil {
		w.msg = nil
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			w.err.Type = ErrTimeout
		}
	}
	return w
}

func drainAndClose(body io.ReadCloser, cancel context.CancelFunc) {
	done := make(chan struct{})
	go func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(body, 1<<20))
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
	}
	_ = body.Close()
	cancel()
}

func (s *Session) withTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	if s.opts.Timeout > 0 {
		return context.WithTimeout(ctx, s.opts.Timeout)
	}
	return context.WithCancel(ctx)
}

func (s *Session) setCommonHeaders(req *http.Request, authHdr string) {
	for k, v := range s.opts.Headers {
		req.Header.Set(k, v)
	}
	if authHdr != "" {
		req.Header.Set("Authorization", authHdr)
	}
	if req.Header.Get("User-Agent") == "" {
		req.Header.Set("User-Agent", "mcpload/"+s.opts.ClientInfo.Version)
	}
}

// authorization obtains the Authorization header value. Options.Timeout
// bounds both how long this caller waits for a token and (unless the auth
// source has its own timeout) how long a token fetch it starts may take.
func (s *Session) authorization(ctx context.Context) (string, *Error) {
	if s.opts.Auth == nil {
		return "", nil
	}
	obs := s.obs(ctx)
	ctx, cancel := s.withTimeout(WithFetchTimeout(ctx, s.opts.Timeout))
	defer cancel()
	v, err := s.opts.Auth.Authorization(ctx, s.hc, obs)
	if err != nil {
		var e *Error
		if errors.As(err, &e) {
			return "", e
		}
		return "", &Error{Type: ErrAuth, Message: err.Error()}
	}
	return v, nil
}

func transportError(ctx context.Context, err error) *Error {
	var ne net.Error
	if errors.Is(err, context.DeadlineExceeded) || ctx.Err() == context.DeadlineExceeded ||
		(errors.As(err, &ne) && ne.Timeout()) {
		return &Error{Type: ErrTimeout, Message: err.Error()}
	}
	return &Error{Type: ErrHTTP, Message: err.Error()}
}

func (s *Session) classifyStatus(resp *http.Response, body []byte, hadSession bool, authHdr string) *Error {
	status := resp.StatusCode
	e := &Error{Type: ErrHTTP, HTTPStatus: status, Message: http.StatusText(status)}
	if re := parseErrorBody(resp.Header.Get("Content-Type"), body); re != nil {
		e.Code, e.Message, e.Data = re.Code, re.Message, re.Data
		if re.Code == CodeHeaderMismatch {
			e.Type = ErrHeaderMismatch
		}
	} else if len(body) > 0 {
		e.Message = truncate(bytes.TrimSpace(body), 300)
	}
	switch {
	case e.Type == ErrHeaderMismatch:
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		e.Type = ErrAuth
		if status == http.StatusUnauthorized && s.opts.Auth != nil {
			s.opts.Auth.Unauthorized(authHdr)
		}
	case status == http.StatusNotFound && hadSession:
		e.Type = ErrSessionNotFound
	}
	return e
}

// parseErrorBody extracts a JSON-RPC error object from a JSON or SSE body.
func parseErrorBody(contentType string, body []byte) *rpcError {
	mt, _, _ := mime.ParseMediaType(contentType)
	if mt == "text/event-stream" {
		msg, err := readSSE(bytes.NewReader(body), "", nil)
		if err != nil || msg == nil {
			return nil
		}
		return msg.Error
	}
	var m rpcMessage
	if json.Unmarshal(bytes.TrimSpace(body), &m) != nil || m.Error == nil {
		return nil
	}
	return m.Error
}

func rpcToError(status int, re *rpcError) *Error {
	t := ErrJSONRPC
	if re.Code == CodeHeaderMismatch {
		t = ErrHeaderMismatch
	}
	return &Error{Type: t, HTTPStatus: status, Code: re.Code, Message: re.Message, Data: re.Data}
}

func idMatches(raw json.RawMessage, want string) bool {
	id := strings.TrimSpace(string(raw))
	return id == want || id == `"`+want+`"`
}

// findResponse locates the response with the wanted id in a JSON body that
// is either a single message or a batch.
func findResponse(body []byte, wantID string) (*rpcMessage, *Error) {
	body = bytes.TrimSpace(body)
	if len(body) > 0 && body[0] == '[' {
		var batch []rpcMessage
		if err := json.Unmarshal(body, &batch); err != nil {
			return nil, &Error{Type: ErrJSONRPC, Message: "decoding batch response: " + err.Error()}
		}
		for i := range batch {
			if idMatches(batch[i].ID, wantID) && batch[i].Method == "" {
				return &batch[i], nil
			}
		}
		return nil, &Error{Type: ErrJSONRPC, Message: "batch response has no message with id " + wantID}
	}
	var m rpcMessage
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, &Error{Type: ErrJSONRPC, Message: "decoding response: " + err.Error()}
	}
	if !idMatches(m.ID, wantID) && !(m.Error != nil && (len(m.ID) == 0 || string(m.ID) == "null")) {
		return nil, &Error{Type: ErrJSONRPC, Message: "response id " + string(m.ID) + " does not match " + wantID}
	}
	return &m, nil
}

// readSSE reads server-sent events until one carries a JSON-RPC response
// whose id equals wantID. With wantID == "" the first response or error
// message is returned. Notifications on the stream are skipped;
// server-to-client requests (a method and a non-null id) are passed to
// onRequest, which must not block, or skipped when it is nil.
func readSSE(r io.Reader, wantID string, onRequest func(*rpcMessage)) (*rpcMessage, *Error) {
	br := bufio.NewReaderSize(r, 32<<10)
	var data strings.Builder
	hasData := false
	dispatch := func() *rpcMessage {
		defer func() { data.Reset(); hasData = false }()
		if !hasData {
			return nil
		}
		var m rpcMessage
		if json.Unmarshal([]byte(data.String()), &m) != nil {
			return nil
		}
		if m.Method != "" { // notification or server request
			if onRequest != nil && hasID(m.ID) {
				onRequest(&m)
			}
			return nil
		}
		if wantID == "" || idMatches(m.ID, wantID) || (m.Error != nil && (len(m.ID) == 0 || string(m.ID) == "null")) {
			return &m
		}
		return nil
	}
	for {
		line, err := br.ReadString('\n')
		if len(line) > 0 {
			line = strings.TrimRight(line, "\r\n")
			switch {
			case line == "":
				if m := dispatch(); m != nil {
					return m, nil
				}
			case strings.HasPrefix(line, ":"):
				// comment / keep-alive
			default:
				field, value, _ := strings.Cut(line, ":")
				value = strings.TrimPrefix(value, " ")
				if field == "data" {
					if hasData {
						data.WriteByte('\n')
					}
					data.WriteString(value)
					hasData = true
				}
			}
		}
		if err != nil {
			if m := dispatch(); m != nil {
				return m, nil
			}
			if errors.Is(err, io.EOF) {
				return nil, &Error{Type: ErrHTTP, Message: "event stream ended without a response for id " + wantID}
			}
			var ne net.Error
			if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout()) {
				return nil, &Error{Type: ErrTimeout, Message: "reading event stream: " + err.Error()}
			}
			return nil, &Error{Type: ErrHTTP, Message: "reading event stream: " + err.Error()}
		}
	}
}
