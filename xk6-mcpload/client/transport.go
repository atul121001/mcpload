package client

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net"
	"net/http"
	"net/http/httptrace"
	"strconv"
	"strings"
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
}

type exchangeResult struct {
	result    json.RawMessage
	sessionID string // Mcp-Session-Id response header
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

	ctx, cancel := s.withTimeout(ctx)
	cancelOwned := true
	defer func() {
		if cancelOwned {
			cancel()
		}
	}()

	// GotFirstResponseByte runs on the transport's goroutine, which can still
	// be running when Do has already returned an error (timeout), so the
	// time is published atomically: nanoseconds since base, 0 = not seen.
	base := time.Now()
	var ttfb atomic.Int64
	trace := &httptrace.ClientTrace{GotFirstResponseByte: func() { ttfb.Store(int64(time.Since(base)) + 1) }}
	req, err := http.NewRequestWithContext(httptrace.WithClientTrace(ctx, trace), http.MethodPost, s.opts.URL, bytes.NewReader(body))
	if err != nil {
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
		}
		s.obs(ctx).OnRequest(res.stats)
	}

	resp, err := s.hc.Do(req)
	if err != nil {
		res.err = transportError(ctx, err)
		finish()
		return res
	}
	headersAt := time.Now()
	res.stats.Status = resp.StatusCode
	res.sessionID = resp.Header.Get(HeaderSessionID)

	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
		_ = resp.Body.Close()
		res.err = s.classifyStatus(resp, b, ex.sessionID != "", authHdr)
		finish()
		return res
	}

	if ex.id == nil { // notification: 202 Accepted (or any 2xx) with no payload
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxErrorBody))
		_ = resp.Body.Close()
		finish()
		return res
	}

	mt, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	wantID := strconv.FormatInt(*ex.id, 10)
	var msg *rpcMessage
	var perr *Error
	if mt == "text/event-stream" {
		res.stats.Streamed = true
		msg, perr = readSSE(resp.Body, wantID)
		res.stats.Stream = time.Since(headersAt)
		// Drain the rest of the stream in the background so the connection
		// can be reused, without holding up the caller or its timings.
		cancelOwned = false
		go drainAndClose(resp.Body, cancel)
	} else {
		b, rerr := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if rerr != nil {
			perr = transportError(ctx, rerr)
		} else {
			msg, perr = findResponse(b, wantID)
		}
	}
	if perr != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			perr.Type = ErrTimeout
		}
		res.err = perr
		finish()
		return res
	}
	if msg.Error != nil {
		res.err = rpcToError(resp.StatusCode, msg.Error)
		finish()
		return res
	}
	res.result = msg.Result
	finish()
	return res
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
		msg, err := readSSE(bytes.NewReader(body), "")
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
// message is returned. Notifications and server-to-client requests on the
// stream are skipped.
func readSSE(r io.Reader, wantID string) (*rpcMessage, *Error) {
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
