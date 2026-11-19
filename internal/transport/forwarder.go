// Package transport forwards the host's streaming transport protocol to HTTP.
package transport

import (
	"container/list"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/net/http/httpguts"
	"golang.org/x/net/proxy"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pluginv1 "sub2api.local/cache-manager/internal/hostapi"
)

const (
	maxCachedClients  = 32
	responseChunkSize = 32 * 1024
)

// Forwarder owns outbound transports and cancels active requests when closed.
// HTTP/1 connections are deliberately not reused: net/http can otherwise replay
// an idempotent request after a stale pooled connection fails.
type Forwarder struct {
	ctx     context.Context
	cancel  context.CancelFunc
	mu      sync.Mutex
	closed  bool
	clients map[string]*list.Element
	lru     list.List
}

type cachedClient struct {
	key       string
	client    *http.Client
	transport *http.Transport
}

type failure struct {
	code    string
	message string
}

func (e *failure) Error() string { return e.message }

var (
	errInvalidRequest = &failure{"INVALID_REQUEST", "Invalid forwarding request."}
	errRequestStream  = &failure{"REQUEST_STREAM_ERROR", "Request stream failed."}
	errUpstream       = &failure{"UPSTREAM_ERROR", "Upstream HTTP request failed."}
	errResponseBody   = &failure{"UPSTREAM_READ_ERROR", "Upstream HTTP response failed."}
	errCanceled       = &failure{"CANCELED", "Forwarding was canceled."}
	errClosed         = &failure{"CLOSED", "Transport is closed."}
	errUploadStopped  = errors.New("request upload stopped")
)

// New creates a forwarder with an empty, bounded outbound client cache.
func New() *Forwarder {
	ctx, cancel := context.WithCancel(context.Background())
	return &Forwarder{ctx: ctx, cancel: cancel, clients: make(map[string]*list.Element)}
}

// Close is idempotent and interrupts active HTTP requests.
func (f *Forwarder) Close() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return
	}
	f.closed = true
	f.cancel()
	for _, element := range f.clients {
		element.Value.(*cachedClient).transport.CloseIdleConnections()
	}
	clear(f.clients)
	f.lru.Init()
}

type incomingFrame struct {
	frame *pluginv1.ForwardRequest
	err   error
}

type uploadResult struct {
	done chan struct{}
	err  error
}

func (u *uploadResult) finish(err error) {
	u.err = err
	close(u.done)
}

// receiveFrames permits handler cancellation while Recv is waiting for a peer.
// gRPC cancels the outstanding Recv when Forward returns. There is at most one
// pending receive and one queued frame per RPC.
func receiveFrames(ctx context.Context, stream pluginv1.TransportPlugin_ForwardServer) <-chan incomingFrame {
	frames := make(chan incomingFrame, 1)
	go func() {
		defer close(frames)
		for {
			frame, err := stream.Recv()
			select {
			case frames <- incomingFrame{frame: frame, err: err}:
			case <-ctx.Done():
				return
			}
			if err != nil {
				return
			}
		}
	}()
	return frames
}

func nextFrame(ctx context.Context, frames <-chan incomingFrame) (*pluginv1.ForwardRequest, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case item, ok := <-frames:
		if !ok {
			return nil, io.EOF
		}
		return item.frame, item.err
	}
}

// Forward performs exactly one outbound HTTP attempt. Errors deliberately use
// fixed messages: underlying HTTP/proxy errors can contain URLs or credentials.
func (f *Forwarder) Forward(stream pluginv1.TransportPlugin_ForwardServer) error {
	started := time.Now()
	ctx, cancel := context.WithCancel(stream.Context())
	stopClose := context.AfterFunc(f.ctx, cancel)
	defer stopClose()
	defer cancel()
	if f.ctx.Err() != nil {
		return sendFailure(stream, errClosed, false)
	}
	frames := receiveFrames(ctx, stream)
	first, err := nextFrame(ctx, frames)
	if err != nil {
		return sendFailure(stream, frameFailure(ctx, err), false)
	}
	if first == nil || first.GetStart() == nil {
		return sendFailure(stream, errInvalidRequest, false)
	}
	start := first.GetStart()
	req, problem := makeRequest(ctx, start)
	if problem != nil {
		return sendFailure(stream, problem, false)
	}
	client, problem := f.client(start.ProxyUrl)
	if problem != nil {
		return sendFailure(stream, problem, false)
	}
	// net/http detaches pending dials from request cancellation for reuse.
	// After Do returns, close abandoned dials as well as idle connections.
	defer client.CloseIdleConnections()

	var reader *io.PipeReader
	var writer *io.PipeWriter
	var bodyEnded atomic.Bool
	upload := &uploadResult{done: make(chan struct{})}
	if start.HasBody {
		reader, writer = io.Pipe()
		req.Body = reader
		defer reader.Close()
		defer writer.Close()
		go func() {
			err := copyRequest(ctx, frames, writer, start, &bodyEnded)
			upload.finish(err)
			if err != nil && !errors.Is(err, errUploadStopped) {
				cancel()
			}
		}()
	} else {
		// With no upload there is no reason to attempt HTTP before validating
		// the terminal frame, including the peer's half-close.
		if err := copyRequest(ctx, frames, nil, start, &bodyEnded); err != nil {
			return sendFailure(stream, frameFailure(ctx, err), false)
		}
		upload.finish(nil)
	}
	if ctx.Err() != nil {
		return sendFailure(stream, uploadFailure(upload, ctx, errCanceled), false)
	}

	// From this point even a dial/TLS failure is conservatively request_sent.
	// No GetBody, redirect following, connection reuse, or HTTP/2 retry path.
	response, err := client.Do(req)
	if err != nil {
		return sendFailure(stream, uploadFailure(upload, ctx, errUpstream), true)
	}
	defer response.Body.Close()
	if problem := completedUploadFailure(upload); problem != nil {
		return sendFailure(stream, problem, true)
	}
	if err := stream.Send(&pluginv1.ForwardResponse{Frame: &pluginv1.ForwardResponse_Start{Start: &pluginv1.ForwardResponseStart{
		StatusCode: int32(response.StatusCode), Status: response.Status,
		Protocol: response.Proto, ProtocolMajor: int32(response.ProtoMajor), ProtocolMinor: int32(response.ProtoMinor),
		Headers: responseHeaders(response.Header), ContentLength: response.ContentLength,
	}}}); err != nil {
		return streamFailure(stream)
	}
	buffer := make([]byte, responseChunkSize)
	var received int64
	for {
		n, readErr := response.Body.Read(buffer)
		if n > 0 {
			chunk := append([]byte(nil), buffer[:n]...)
			if err := stream.Send(&pluginv1.ForwardResponse{Frame: &pluginv1.ForwardResponse_BodyChunk{BodyChunk: chunk}}); err != nil {
				return streamFailure(stream)
			}
			received += int64(n)
		}
		if readErr != nil {
			if readErr != io.EOF {
				return sendFailure(stream, uploadFailure(upload, ctx, errResponseBody), true)
			}
			break
		}
	}
	// Once body_end was received, validate EOF and reject any trailing frame.
	// An early upstream response must not wait for the unfinished upload.
	if bodyEnded.Load() {
		select {
		case <-upload.done:
			if upload.err != nil && !errors.Is(upload.err, errUploadStopped) {
				return sendFailure(stream, frameFailure(ctx, upload.err), true)
			}
		case <-ctx.Done():
			return sendFailure(stream, uploadFailure(upload, ctx, errCanceled), true)
		}
	} else if problem := completedUploadFailure(upload); problem != nil {
		return sendFailure(stream, problem, true)
	}
	if err := stream.Send(&pluginv1.ForwardResponse{Frame: &pluginv1.ForwardResponse_End{End: &pluginv1.ForwardResponseEnd{
		BytesReceived: received, DurationMs: time.Since(started).Milliseconds(),
	}}}); err != nil {
		return streamFailure(stream)
	}
	return nil
}

func makeRequest(ctx context.Context, start *pluginv1.ForwardRequestStart) (*http.Request, *failure) {
	if start.Method == "" || start.ContentLength < -1 || (!start.HasBody && start.ContentLength > 0) {
		return nil, errInvalidRequest
	}
	u, err := url.Parse(start.Url)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil || u.Fragment != "" {
		return nil, errInvalidRequest
	}
	if !httpguts.ValidHostHeader(u.Host) || (start.Host != "" && !httpguts.ValidHostHeader(start.Host)) {
		return nil, errInvalidRequest
	}
	req, err := http.NewRequestWithContext(ctx, start.Method, start.Url, nil)
	if err != nil {
		return nil, errInvalidRequest
	}
	for name, values := range start.Headers {
		if !httpguts.ValidHeaderFieldName(name) || values == nil {
			return nil, errInvalidRequest
		}
		for _, value := range values.Values {
			if !httpguts.ValidHeaderFieldValue(value) {
				return nil, errInvalidRequest
			}
			req.Header.Add(name, value)
		}
	}
	req.Host = start.Host
	if start.HasBody {
		req.ContentLength = start.ContentLength
	}
	return req, nil
}

func copyRequest(ctx context.Context, frames <-chan incomingFrame, writer *io.PipeWriter, start *pluginv1.ForwardRequestStart, ended *atomic.Bool) (result error) {
	if writer != nil {
		defer func() { _ = writer.CloseWithError(result) }()
	}
	var received int64
	for {
		frame, err := nextFrame(ctx, frames)
		if err != nil {
			if err == io.EOF && ended.Load() {
				return nil
			}
			if err == io.EOF {
				return errInvalidRequest
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return errRequestStream
		}
		if frame == nil || ended.Load() {
			return errInvalidRequest
		}
		switch value := frame.Frame.(type) {
		case *pluginv1.ForwardRequest_BodyChunk:
			if !start.HasBody {
				return errInvalidRequest
			}
			received += int64(len(value.BodyChunk))
			if start.ContentLength > 0 && received > start.ContentLength {
				return errInvalidRequest
			}
			if len(value.BodyChunk) > 0 {
				if _, err := writer.Write(value.BodyChunk); err != nil {
					return errUploadStopped
				}
			}
		case *pluginv1.ForwardRequest_BodyEnd:
			if !value.BodyEnd || (start.ContentLength > 0 && received != start.ContentLength) {
				return errInvalidRequest
			}
			ended.Store(true)
			if writer != nil {
				_ = writer.Close()
			}
		default:
			return errInvalidRequest
		}
	}
}

func (f *Forwarder) client(proxyURL string) (*http.Client, *failure) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return nil, errClosed
	}
	if element, ok := f.clients[proxyURL]; ok {
		f.lru.MoveToFront(element)
		return element.Value.(*cachedClient).client, nil
	}
	dialer := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: -1}
	protocols := new(http.Protocols)
	protocols.SetHTTP1(true)
	transport := &http.Transport{
		DialContext: dialer.DialContext, DisableCompression: true,
		DisableKeepAlives: true, Protocols: protocols,
		TLSHandshakeTimeout: 10 * time.Second, ExpectContinueTimeout: time.Second,
		MaxResponseHeaderBytes: 1 << 20,
	}
	if proxyURL != "" {
		u, err := url.Parse(proxyURL)
		if err != nil || u.Hostname() == "" || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") || !httpguts.ValidHostHeader(u.Host) {
			return nil, errInvalidRequest
		}
		switch strings.ToLower(u.Scheme) {
		case "http", "https":
			transport.Proxy = http.ProxyURL(u)
		case "socks5", "socks5h":
			address := u.Host
			if u.Port() == "" {
				address = net.JoinHostPort(u.Hostname(), "1080")
			}
			var auth *proxy.Auth
			if u.User != nil {
				password, _ := u.User.Password()
				auth = &proxy.Auth{User: u.User.Username(), Password: password}
			}
			socks, err := proxy.SOCKS5("tcp", address, auth, dialer)
			if err != nil {
				return nil, errInvalidRequest
			}
			contextDialer, ok := socks.(proxy.ContextDialer)
			if !ok {
				return nil, errInvalidRequest
			}
			// Like Go's HTTP transport, both SOCKS schemes resolve target
			// hostnames at the proxy; no credential-bearing fallback dial.
			transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
				dialCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
				defer cancel()
				return contextDialer.DialContext(dialCtx, network, address)
			}
		default:
			return nil, errInvalidRequest
		}
	}
	client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	element := f.lru.PushFront(&cachedClient{key: proxyURL, client: client, transport: transport})
	f.clients[proxyURL] = element
	if f.lru.Len() > maxCachedClients {
		oldest := f.lru.Back()
		entry := oldest.Value.(*cachedClient)
		delete(f.clients, entry.key)
		f.lru.Remove(oldest)
		entry.transport.CloseIdleConnections()
	}
	return client, nil
}

func responseHeaders(headers http.Header) map[string]*pluginv1.HeaderValues {
	result := make(map[string]*pluginv1.HeaderValues, len(headers))
	for name, values := range headers {
		result[name] = &pluginv1.HeaderValues{Values: append([]string(nil), values...)}
	}
	return result
}

func completedUploadFailure(upload *uploadResult) *failure {
	select {
	case <-upload.done:
		if upload.err != nil && !errors.Is(upload.err, errUploadStopped) {
			return frameFailure(context.Background(), upload.err)
		}
	default:
	}
	return nil
}

func uploadFailure(upload *uploadResult, ctx context.Context, fallback *failure) *failure {
	if problem := completedUploadFailure(upload); problem != nil {
		return problem
	}
	if ctx.Err() != nil {
		return errCanceled
	}
	return fallback
}

func frameFailure(ctx context.Context, err error) *failure {
	var problem *failure
	if errors.As(err, &problem) {
		return problem
	}
	if ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return errCanceled
	}
	if err == io.EOF {
		return errInvalidRequest
	}
	return errRequestStream
}

func sendFailure(stream pluginv1.TransportPlugin_ForwardServer, problem *failure, sent bool) error {
	if err := stream.Send(&pluginv1.ForwardResponse{Frame: &pluginv1.ForwardResponse_Error{Error: &pluginv1.ForwardResponseError{
		Code: problem.code, Message: problem.message, RequestSent: sent,
	}}}); err != nil {
		return streamFailure(stream)
	}
	return nil
}

func streamFailure(stream pluginv1.TransportPlugin_ForwardServer) error {
	if err := stream.Context().Err(); err != nil {
		return status.FromContextError(err).Err()
	}
	return status.Error(codes.Unavailable, "Transport stream failed.")
}
