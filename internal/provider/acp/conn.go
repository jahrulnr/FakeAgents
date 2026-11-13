package acp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
)

// ErrClosed reports use of a closed connection.
var ErrClosed = errors.New("acp: connection closed")

// maxLineBytes bounds one NDJSON frame (32 MiB, matching server.max_body_bytes
// scale for the HTTP surface).
const maxLineBytes = 32 << 20

// conn is a newline-delimited JSON-RPC 2.0 connection. One reader goroutine
// classifies inbound frames; inbound requests and notifications dispatch to
// handler goroutines; inbound responses resolve entries in the pending map.
// All writes serialize through a single mutex.
type conn struct {
	out     io.Writer
	wmu     sync.Mutex
	logger  *slog.Logger
	handler func(ctx context.Context, msg *rpcMessage)

	nextID  atomic.Int64
	pmu     sync.Mutex
	pending map[string]chan *rpcMessage

	ctx      context.Context
	cancel   context.CancelFunc
	once     sync.Once
	done     chan struct{}
	inCloser io.Closer
	handlers sync.WaitGroup
}

func newConn(ctx context.Context, out io.Writer, handler func(context.Context, *rpcMessage), logger *slog.Logger) *conn {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	cctx, cancel := context.WithCancel(ctx)
	c := &conn{
		out:     out,
		logger:  logger,
		handler: handler,
		pending: make(map[string]chan *rpcMessage),
		ctx:     cctx,
		cancel:  cancel,
		done:    make(chan struct{}),
	}
	// Parent cancellation (SIGTERM, Run ctx) must unblock the read loop even
	// when the peer stays silent, so close the connection from a watcher.
	go func() {
		select {
		case <-cctx.Done():
			c.Close()
		case <-c.done:
		}
	}()
	return c
}

// run reads frames until EOF, an unrecoverable write failure, or Close.
// Frames accumulate one buffer slice at a time so the maxLineBytes bound
// applies during reads, not after. On EOF pending agent->client calls are
// failed (their replies can never arrive) and in-flight handlers are awaited,
// so a client that writes its last request and immediately closes stdin still
// gets the response.
func (c *conn) run(in io.Reader) error {
	defer close(c.done)
	if closer, ok := in.(io.Closer); ok {
		c.pmu.Lock()
		c.inCloser = closer
		c.pmu.Unlock()
	}
	reader := bufio.NewReaderSize(in, 64<<10)
	var line []byte
	oversized := false
	for {
		frag, err := reader.ReadSlice('\n')
		if oversized {
			// Discard the remainder of an oversized frame until its newline.
			if err == bufio.ErrBufferFull {
				continue
			}
			oversized = false
		} else {
			line = append(line, frag...)
			if err == bufio.ErrBufferFull {
				if len(line) > maxLineBytes {
					c.write(response(nil, nil, rpcErr(codeInvalidRequest, "frame exceeds maximum size", nil)))
					oversized = true
					line = line[:0]
				}
				continue
			}
		}
		if len(line) > 0 {
			c.line(line)
			line = line[:0]
		}
		if err != nil {
			c.failPending()
			c.handlers.Wait()
			if errors.Is(err, io.EOF) || c.ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("acp: read: %w", err)
		}
	}
}

// line handles one complete frame. A frame delivered without a trailing
// newline (EOF cut it short) is still valid and processed the same way.
func (c *conn) line(line []byte) {
	trimmed := trimLine(line)
	if len(trimmed) == 0 {
		return
	}
	if trimmed[0] == '[' {
		// JSON-RPC batches are not part of ACP.
		c.write(response(nil, nil, rpcErr(codeInvalidRequest, "batch requests are not supported", nil)))
		return
	}
	var msg rpcMessage
	if err := json.Unmarshal(trimmed, &msg); err != nil {
		c.write(response(nil, nil, rpcErr(codeParseError, "invalid JSON: "+err.Error(), nil)))
		return
	}
	switch {
	case msg.isResponse():
		c.resolve(&msg)
	case msg.isRequest():
		c.handlers.Add(1)
		go c.serve(&msg)
	default:
		// Notification: dispatch and never respond.
		if msg.Method == "" {
			// Invalid request object: neither response, request, nor notification.
			c.write(response(nil, nil, rpcErr(codeInvalidRequest, "message is not a valid JSON-RPC object", nil)))
			return
		}
		c.handlers.Add(1)
		go c.serve(&msg)
	}
}

// serve invokes the agent handler in a guarded goroutine.
func (c *conn) serve(msg *rpcMessage) {
	defer c.handlers.Done()
	defer func() {
		if recovered := recover(); recovered != nil {
			c.logger.Error("acp: handler panic", "error", recovered, "method", msg.Method)
			if msg.isRequest() {
				c.write(response(msg.ID, nil, rpcErrInternal(fmt.Sprintf("internal error: %v", recovered))))
			}
		}
	}()
	c.handler(c.ctx, msg)
}

// write serialises one outbound message. Failures are logged; a broken pipe
// shows up on the next read as EOF.
func (c *conn) write(msg *rpcMessage) bool {
	data, err := json.Marshal(msg)
	if err != nil {
		c.logger.Error("acp: encode message", "error", err)
		return false
	}
	return c.writeRaw(data)
}

func (c *conn) writeRaw(data []byte) bool {
	select {
	case <-c.ctx.Done():
		return false
	default:
	}
	data = append(data, '\n')
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if _, err := c.out.Write(data); err != nil {
		c.logger.Error("acp: write", "error", err)
		c.Close()
		return false
	}
	return true
}

// call sends an outbound request and waits for the matching response.
func (c *conn) call(ctx context.Context, method string, params any, result any) *rpcError {
	id := json.RawMessage(fmt.Sprintf(`"agent-%d"`, c.nextID.Add(1)))
	ch := make(chan *rpcMessage, 1)
	key := string(id)
	c.pmu.Lock()
	c.pending[key] = ch
	c.pmu.Unlock()
	defer func() {
		c.pmu.Lock()
		delete(c.pending, key)
		c.pmu.Unlock()
	}()

	body, err := json.Marshal(params)
	if err != nil {
		return rpcErrInternal("encode params: " + err.Error())
	}
	msg := &rpcMessage{JSONRPC: "2.0", ID: id, Method: method, Params: body}
	if !c.write(msg) {
		return rpcErrInternal("write request failed")
	}

	select {
	case res := <-ch:
		if res.Error != nil {
			return res.Error
		}
		if result != nil && len(res.Result) > 0 {
			if err := json.Unmarshal(res.Result, result); err != nil {
				return rpcErrInternal("decode result: " + err.Error())
			}
		}
		return nil
	case <-ctx.Done():
		return rpcErr(codeCancelled, "request cancelled", nil)
	case <-c.ctx.Done():
		return rpcErrInternal("connection closed")
	}
}

// notify sends a notification (no id, no response expected).
func (c *conn) notify(method string, params any) bool {
	body, err := json.Marshal(params)
	if err != nil {
		c.logger.Error("acp: encode notification", "error", err, "method", method)
		return false
	}
	return c.write(&rpcMessage{JSONRPC: "2.0", Method: method, Params: body})
}

// resolve delivers an inbound response to its waiting call.
func (c *conn) resolve(msg *rpcMessage) {
	key := string(msg.ID)
	c.pmu.Lock()
	ch, ok := c.pending[key]
	c.pmu.Unlock()
	if !ok {
		c.logger.Debug("acp: response for unknown id", "id", key)
		return
	}
	select {
	case ch <- msg:
	default:
	}
}

// failPending releases every goroutine blocked in call with a synthetic
// error. Used both on Close and on EOF, where replies can no longer arrive.
func (c *conn) failPending() {
	c.pmu.Lock()
	for key, ch := range c.pending {
		delete(c.pending, key)
		select {
		case ch <- &rpcMessage{Error: rpcErrInternal("connection closed")}:
		default:
		}
	}
	c.pmu.Unlock()
}

// Close ends the connection: readers see EOF behaviour, pending callers are
// released with an internal error, and handler contexts cancel. Closing the
// input unblocks the read loop, so Run can return (and the CLI exit).
func (c *conn) Close() {
	c.once.Do(func() {
		c.cancel()
		c.failPending()
		c.pmu.Lock()
		inCloser := c.inCloser
		c.pmu.Unlock()
		if inCloser != nil {
			_ = inCloser.Close()
		}
	})
}

func trimLine(line []byte) []byte {
	for len(line) > 0 && (line[len(line)-1] == '\n' || line[len(line)-1] == '\r' || line[len(line)-1] == ' ' || line[len(line)-1] == '\t') {
		line = line[:len(line)-1]
	}
	for len(line) > 0 && (line[0] == ' ' || line[0] == '\t') {
		line = line[1:]
	}
	return line
}
