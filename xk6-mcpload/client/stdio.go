package client

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// stdio transport (MCP "stdio"): the client launches the server as a
// subprocess and exchanges newline-delimited JSON-RPC messages over its stdin
// and stdout. stderr is the server's log; its tail is kept for error
// messages. One Session is one process, as in desktop MCP clients.
//
// Everything above the wire (handshake, tools, resources, prompts,
// CallParallel, metrics) is shared with HTTP: Session.post sends an exchange
// over stdio when the session has a process (stdioPost).
//
// Differences from HTTP:
//   - Protocol "auto" runs initialize directly: the server/discover probe is
//     an HTTP transport rule. Explicit versions are still honoured.
//   - Status is always 0 and there is no TTFB.
//   - Cancellation always sends notifications/cancelled on stdin, and a late
//     response is observed on stdout within CancelWait (same pipe).
//   - Server-to-client requests can arrive at any time; they are answered on
//     stdin, attributed to the oldest pending request.
//   - Lines on stdout that are not JSON-RPC messages (a log written to stdout
//     corrupts the protocol) are skipped and reported (StdoutInvalidStats).
//   - When the process exits, every pending request fails with
//     ErrProcessExit (exit code and stderr tail in the message).

const (
	// maxStdioLine bounds one message on stdout; longer lines are skipped
	// (and reported as invalid).
	maxStdioLine = 16 << 20
	// stderrKeep is how much of the server's stderr is kept.
	stderrKeep = 64 << 10
	// stdioCloseGrace is how long Close waits for the process to exit after
	// closing its stdin before killing it.
	stdioCloseGrace = 2 * time.Second
	// stderrTailInError is how much of the stderr tail goes into an error.
	stderrTailInError = 2 << 10
)

// ProcessStats describes the start of a stdio server process.
type ProcessStats struct {
	PID       int    // 0 when the process did not start
	Program   string // base name of Command[0]
	ErrorType string // ErrProcessSpawn when it did not start
	Start     time.Time
	Spawn     time.Duration // until the process was started (not until it answered)
}

// ProcessExitStats describes the exit of a stdio server process.
type ProcessExitStats struct {
	PID      int
	ExitCode int // -1 when killed by a signal
	// Expected is set when the exit followed Session.Close (or a failed
	// connect); otherwise the process died on its own.
	Expected   bool
	Lifetime   time.Duration
	StderrTail string
}

// StdoutInvalidStats describes one line on the server's stdout that is not a
// JSON-RPC message.
type StdoutInvalidStats struct {
	PID    int
	Sample string // the line, truncated to 200 bytes
}

// ProcessObserver is optionally implemented by an Observer (the one in
// effect at Connect) to receive stdio process events. OnProcessExit and
// OnStdoutInvalid are called from the session's own goroutines and may come
// after the call that caused them returned.
type ProcessObserver interface {
	OnProcessSpawn(ProcessStats)
	OnProcessExit(ProcessExitStats)
	OnStdoutInvalid(StdoutInvalidStats)
}

type stdioPending struct {
	ch  chan *rpcMessage // buffered: the reader never blocks on it
	ctx context.Context
	ex  exchange
}

// stdioConn is one server process and its pipes.
type stdioConn struct {
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	stdout  *os.File
	stderr  *tailBuffer
	pid     int
	started time.Time
	tree    *processTree
	obs     Observer

	wmu sync.Mutex // one message per write

	mu      sync.Mutex
	pending map[int64]*stdioPending

	readDone chan struct{}
	dead     chan struct{} // closed once the process has exited; exitErr is set before
	exitErr  *Error
	closing  atomic.Bool
	invalid  atomic.Int64

	// onServerRequest answers a server-to-client request (set by the Session).
	onServerRequest func(ctx context.Context, ex exchange, m *rpcMessage)
}

// startStdio starts opts.Command. The process is placed in its own process
// group (Unix) or job object (Windows) so that Close kills the whole tree,
// including children of launchers such as npx or uvx.
func startStdio(opts Options, obs Observer) (*stdioConn, *Error) {
	ps := ProcessStats{Program: filepath.Base(opts.Command[0]), Start: time.Now()}
	po, _ := obs.(ProcessObserver)
	fail := func(msg string, err error) *Error {
		ps.Spawn, ps.ErrorType = time.Since(ps.Start), ErrProcessSpawn
		if po != nil {
			po.OnProcessSpawn(ps)
		}
		return &Error{Type: ErrProcessSpawn, Message: fmt.Sprintf("%s %q: %v", msg, opts.Command[0], err)}
	}

	cmd := exec.Command(opts.Command[0], opts.Command[1:]...)
	cmd.Dir = opts.Dir
	cmd.Env = stdioEnv(opts.Env)
	setProcAttrs(cmd)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fail("creating stdin pipe for", err)
	}
	// stdout is an *os.File so that exec hands it to the child directly: no
	// copying goroutine, and Wait doesn't close our read end under the reader.
	pr, pw, err := os.Pipe()
	if err != nil {
		return nil, fail("creating stdout pipe for", err)
	}
	cmd.Stdout = pw
	tail := &tailBuffer{max: stderrKeep}
	cmd.Stderr = tail
	// Wait returns at most this long after the process exited even when a
	// grandchild still holds stderr open.
	cmd.WaitDelay = stdioCloseGrace
	if err := cmd.Start(); err != nil {
		_ = pr.Close()
		_ = pw.Close()
		return nil, fail("starting", err)
	}
	_ = pw.Close()
	ps.Spawn, ps.PID = time.Since(ps.Start), cmd.Process.Pid
	c := &stdioConn{
		cmd: cmd, stdin: stdin, stdout: pr, stderr: tail, pid: cmd.Process.Pid, started: ps.Start,
		tree: attachProcessTree(cmd), obs: obs,
		pending: map[int64]*stdioPending{}, readDone: make(chan struct{}), dead: make(chan struct{}),
	}
	if po != nil {
		po.OnProcessSpawn(ps)
	}
	go c.read()
	go c.wait()
	return c, nil
}

// withheldEnv are mcpload's own credentials: the upload key and the HTTP
// target's token and OAuth secret. They mean nothing to a stdio server, so
// they are not inherited (Options.Env can still pass them on purpose).
var withheldEnv = []string{"MCPLOAD_KEY", "MCP_TOKEN", "OAUTH_CLIENT_SECRET"}

// stdioEnv is the parent's environment, without withheldEnv, plus env
// (sorted, so that the result is deterministic; exec keeps the last value of
// a duplicated key).
func stdioEnv(env map[string]string) []string {
	var out []string
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		withheld := false
		for _, w := range withheldEnv {
			// Windows environment names are case-insensitive.
			if strings.EqualFold(name, w) {
				withheld = true
				break
			}
		}
		if !withheld {
			out = append(out, kv)
		}
	}
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		out = append(out, k+"="+env[k])
	}
	return out
}

func (c *stdioConn) register(id int64, ctx context.Context, ex exchange) *stdioPending {
	p := &stdioPending{ch: make(chan *rpcMessage, 1), ctx: ctx, ex: ex}
	c.mu.Lock()
	c.pending[id] = p
	c.mu.Unlock()
	return p
}

func (c *stdioConn) unregister(id int64) {
	c.mu.Lock()
	delete(c.pending, id)
	c.mu.Unlock()
}

// oldestPending returns the context and exchange of the pending request with
// the lowest id, to attribute a server-to-client request to.
func (c *stdioConn) oldestPending() (context.Context, exchange, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var best *stdioPending
	var bestID int64
	for id, p := range c.pending {
		if best == nil || id < bestID {
			best, bestID = p, id
		}
	}
	if best == nil {
		return nil, exchange{}, false
	}
	return best.ctx, best.ex, true
}

// write sends one message.
func (c *stdioConn) write(msg any) *Error {
	b, err := json.Marshal(msg)
	if err != nil {
		return &Error{Type: ErrJSONRPC, Message: "encoding message: " + err.Error()}
	}
	b = append(b, '\n')
	select {
	case <-c.dead:
		return c.exitError()
	default:
	}
	c.wmu.Lock()
	_, err = c.stdin.Write(b)
	c.wmu.Unlock()
	if err != nil {
		// Usually the process is gone: wait briefly for its exit status.
		select {
		case <-c.dead:
			return c.exitError()
		case <-time.After(500 * time.Millisecond):
		}
		return &Error{Type: ErrProcessExit, Message: "writing to the server's stdin: " + err.Error()}
	}
	return nil
}

// exitError returns a copy of the process-exit error (callers may modify it).
func (c *stdioConn) exitError() *Error {
	e := *c.exitErr
	return &e
}

// read dispatches the messages on stdout until it ends.
func (c *stdioConn) read() {
	defer close(c.readDone)
	br := bufio.NewReaderSize(c.stdout, 64<<10)
	for {
		line, tooLong, err := readLine(br, maxStdioLine)
		if tooLong {
			c.invalidLine([]byte(fmt.Sprintf("<line longer than %d bytes>", maxStdioLine)))
		} else if len(line) > 0 {
			c.dispatch(line)
		}
		if err != nil {
			break
		}
	}
	// No more messages can arrive: make sure the process is gone too, so that
	// wait() fails the pending requests.
	if !c.closing.Load() {
		c.tree.kill()
	}
}

// readLine reads one line without its line ending. A line longer than max is
// discarded (tooLong).
func readLine(br *bufio.Reader, max int) (line []byte, tooLong bool, err error) {
	for {
		frag, e := br.ReadSlice('\n')
		if !tooLong {
			if len(line)+len(frag) > max+2 { // +2: the line ending
				tooLong, line = true, nil
			} else {
				line = append(line, frag...)
			}
		}
		if e == bufio.ErrBufferFull {
			continue
		}
		if tooLong {
			return nil, true, e
		}
		return bytes.TrimRight(line, "\r\n"), false, e
	}
}

func (c *stdioConn) dispatch(line []byte) {
	t := bytes.TrimSpace(line)
	if len(t) == 0 {
		return
	}
	var msgs []rpcMessage
	if t[0] == '[' {
		if json.Unmarshal(t, &msgs) != nil || len(msgs) == 0 {
			c.invalidLine(line)
			return
		}
	} else {
		var m rpcMessage
		if json.Unmarshal(t, &m) != nil {
			c.invalidLine(line)
			return
		}
		msgs = []rpcMessage{m}
	}
	for i := range msgs {
		m := &msgs[i]
		// A JSON log line ({"level":"info",...}) is not a message either.
		if m.JSONRPC != "2.0" || (m.Method == "" && m.Result == nil && m.Error == nil) {
			c.invalidLine(line)
			continue
		}
		switch {
		case m.Method != "" && hasID(m.ID):
			c.serverRequest(m)
		case m.Method != "":
			// notification (log, progress, list_changed): ignored
		default:
			c.deliver(m)
		}
	}
}

func (c *stdioConn) deliver(m *rpcMessage) {
	raw := strings.Trim(strings.TrimSpace(string(m.ID)), `"`)
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return // id null (an error the server could not attribute) or not ours
	}
	c.mu.Lock()
	p := c.pending[id]
	delete(c.pending, id)
	c.mu.Unlock()
	if p != nil {
		p.ch <- m
	}
	// else: a response after its request gave up (timeout): ignored.
}

func (c *stdioConn) serverRequest(m *rpcMessage) {
	if c.onServerRequest == nil {
		return
	}
	ctx, ex, ok := c.oldestPending()
	if !ok {
		ctx = context.Background()
	}
	go c.onServerRequest(ctx, ex, m)
}

func (c *stdioConn) invalidLine(line []byte) {
	c.invalid.Add(1)
	if po, ok := c.obs.(ProcessObserver); ok {
		po.OnStdoutInvalid(StdoutInvalidStats{PID: c.pid, Sample: truncate(bytes.TrimSpace(line), 200)})
	}
}

// wait reaps the process and fails everything still pending.
func (c *stdioConn) wait() {
	err := c.cmd.Wait()
	code := -1
	state := ""
	if c.cmd.ProcessState != nil {
		code = c.cmd.ProcessState.ExitCode()
		state = c.cmd.ProcessState.String()
	} else if err != nil {
		state = err.Error()
	}
	// Children that outlived the server (and may hold stdout open) go too.
	c.tree.kill()
	select {
	case <-c.readDone:
	case <-time.After(stdioCloseGrace):
		_ = c.stdout.Close()
	}
	tail := c.stderr.tail(stderrTailInError)
	msg := "server process exited (" + state + ")"
	if !c.closing.Load() && tail != "" {
		msg += "; stderr: " + tail
	}
	c.exitErr = &Error{Type: ErrProcessExit, Message: msg}
	close(c.dead)
	c.tree.release()
	if po, ok := c.obs.(ProcessObserver); ok {
		po.OnProcessExit(ProcessExitStats{PID: c.pid, ExitCode: code, Expected: c.closing.Load(),
			Lifetime: time.Since(c.started), StderrTail: tail})
	}
}

// close ends the process: stdin is closed (the spec's shutdown signal), then
// the process tree is killed if it hasn't exited within stdioCloseGrace.
func (c *stdioConn) close() {
	c.closing.Store(true)
	c.wmu.Lock()
	_ = c.stdin.Close()
	c.wmu.Unlock()
	t := time.NewTimer(stdioCloseGrace)
	defer t.Stop()
	select {
	case <-c.dead:
		return
	case <-t.C:
	}
	c.tree.kill()
	_ = c.cmd.Process.Kill()
	<-c.dead
}

// stdioPost is post over stdio: write the message, then wait for the
// response with the same id.
func (s *Session) stdioPost(ctx context.Context, ex exchange) exchangeResult {
	c := s.stdio
	res := exchangeResult{stats: RequestStats{Method: ex.method, Tool: ex.tool, Protocol: s.protocolTag(ex), Transport: TransportStdio}}
	var p *stdioPending
	if ex.id != nil {
		p = c.register(*ex.id, ctx, ex)
	}
	start := time.Now()
	res.stats.Start = start
	finish := func() {
		res.stats.Duration = time.Since(start)
		if res.err != nil {
			res.stats.ErrorType = res.err.Type
		}
		s.obs(ctx).OnRequest(res.stats)
	}
	if werr := c.write(rpcRequest{JSONRPC: "2.0", ID: ex.id, Method: ex.method, Params: ex.params}); werr != nil {
		if p != nil {
			c.unregister(*ex.id)
		}
		res.err = werr
		finish()
		return res
	}
	if p == nil { // notification
		finish()
		return res
	}

	tctx, cancel := s.withTimeout(ctx)
	defer cancel()
	var cancelC <-chan time.Time
	if ex.cancelAfter > 0 {
		t := time.NewTimer(ex.cancelAfter)
		defer t.Stop()
		cancelC = t.C
	}
	settle := func(m *rpcMessage) {
		if m.Error != nil {
			res.err = rpcToError(0, m.Error)
		} else {
			res.result = m.Result
		}
	}
	select {
	case m := <-p.ch:
		settle(m)
	case <-c.dead:
		select {
		case m := <-p.ch: // answered just before the exit
			settle(m)
		default:
			c.unregister(*ex.id)
			res.err = c.exitError()
		}
	case <-tctx.Done():
		c.unregister(*ex.id)
		select {
		case m := <-p.ch:
			settle(m)
		default:
			res.err = transportError(tctx, tctx.Err())
		}
	case <-cancelC:
		res.err = &Error{Type: ErrCancelled, Message: fmt.Sprintf("cancelled by the client after %s", ex.cancelAfter)}
		finish()
		s.stdioCancelInFlight(ctx, ex, p)
		return res
	}
	finish()
	if ex.cancelAfter > 0 {
		s.reportCancel(ctx, CancelStats{Method: ex.method, Tool: ex.tool, Protocol: s.protocolTag(ex),
			Reason: CancelReasonClient, Outcome: CancelOutcomeCompleted, Start: time.Now()})
	}
	s.cancelOnTimeout(ctx, ex, res.err)
	return res
}

// stdioCancelInFlight sends notifications/cancelled for a call that is still
// pending and watches stdout for a late response for CancelWait. The cancel
// event is reported once the outcome is known.
func (s *Session) stdioCancelInFlight(ctx context.Context, ex exchange, p *stdioPending) {
	c := s.stdio
	cs := CancelStats{Method: ex.method, Tool: ex.tool, Protocol: s.protocolTag(ex), Reason: CancelReasonClient, Start: time.Now()}
	n := s.post(ctx, exchange{
		method: "notifications/cancelled", protoHdr: ex.protoHdr,
		params: map[string]any{"requestId": *ex.id, "reason": fmt.Sprintf("mcpload: cancelled after %s", ex.cancelAfter)},
	})
	cs.Duration = time.Since(cs.Start)
	if n.err != nil {
		c.unregister(*ex.id)
		cs.Outcome, cs.ErrorType = CancelOutcomeSendFailed, n.err.Type
		s.reportCancel(ctx, cs)
		return
	}
	go func() {
		t := time.NewTimer(s.cancelWait())
		defer t.Stop()
		cs.Outcome = CancelOutcomeCancelled
		select {
		case <-p.ch:
			cs.Outcome, cs.LateAfter = CancelOutcomeLateResponse, time.Since(cs.Start)
		case <-t.C:
			c.unregister(*ex.id)
		case <-c.dead:
			c.unregister(*ex.id)
		}
		s.reportCancel(ctx, cs)
	}()
}

// tailBuffer keeps the last max bytes written to it.
type tailBuffer struct {
	mu  sync.Mutex
	max int
	buf []byte
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if over := len(t.buf) - t.max; over > 0 {
		t.buf = append(t.buf[:0], t.buf[over:]...)
	}
	return len(p), nil
}

// tail returns up to the last n bytes, starting at a line boundary when
// possible, with surrounding whitespace removed.
func (t *tailBuffer) tail(n int) string {
	t.mu.Lock()
	defer t.mu.Unlock()
	b := t.buf
	if len(b) > n {
		b = b[len(b)-n:]
		if i := bytes.IndexByte(b, '\n'); i >= 0 && i < len(b)-1 {
			b = b[i+1:]
		}
	}
	return strings.TrimSpace(string(b))
}

// PID returns the process id of the stdio server process, or 0 for an HTTP
// session.
func (s *Session) PID() int {
	if s.stdio == nil {
		return 0
	}
	return s.stdio.pid
}

// StdoutInvalidLines returns how many lines on the stdio server's stdout were
// not JSON-RPC messages (0 for an HTTP session).
func (s *Session) StdoutInvalidLines() int64 {
	if s.stdio == nil {
		return 0
	}
	return s.stdio.invalid.Load()
}

// Transport returns TransportStdio or TransportHTTP.
func (s *Session) Transport() string {
	if s.stdio != nil {
		return TransportStdio
	}
	return TransportHTTP
}
