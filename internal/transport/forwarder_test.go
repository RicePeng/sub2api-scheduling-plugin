package transport

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	pluginv1 "sub2api.local/cache-manager/internal/hostapi"
)

type rpcServer struct {
	pluginv1.UnimplementedTransportPluginServer
	forwarder *Forwarder
}

func (s *rpcServer) Forward(stream pluginv1.TransportPlugin_ForwardServer) error {
	return s.forwarder.Forward(stream)
}

func rpcClient(t *testing.T, f *Forwarder) pluginv1.TransportPluginClient {
	t.Helper()
	lis := bufconn.Listen(1024 * 1024)
	server := grpc.NewServer()
	pluginv1.RegisterTransportPluginServer(server, &rpcServer{forwarder: f})
	go func() { _ = server.Serve(lis) }()
	conn, err := grpc.NewClient("passthrough:///transport-test", grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) {
		return lis.Dial()
	}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close(); server.Stop(); _ = lis.Close(); f.Close() })
	return pluginv1.NewTransportPluginClient(conn)
}

func startFrame(target string, hasBody bool, length int64) *pluginv1.ForwardRequest {
	return &pluginv1.ForwardRequest{Frame: &pluginv1.ForwardRequest_Start{Start: &pluginv1.ForwardRequestStart{
		Method: "POST", Url: target, HasBody: hasBody, ContentLength: length,
	}}}
}

func bodyFrame(body []byte) *pluginv1.ForwardRequest {
	return &pluginv1.ForwardRequest{Frame: &pluginv1.ForwardRequest_BodyChunk{BodyChunk: body}}
}

func endFrame() *pluginv1.ForwardRequest {
	return &pluginv1.ForwardRequest{Frame: &pluginv1.ForwardRequest_BodyEnd{BodyEnd: true}}
}

func sendEmpty(t *testing.T, stream pluginv1.TransportPlugin_ForwardClient, first *pluginv1.ForwardRequest) {
	t.Helper()
	for _, frame := range []*pluginv1.ForwardRequest{first, endFrame()} {
		if err := stream.Send(frame); err != nil {
			t.Fatal(err)
		}
	}
	if err := stream.CloseSend(); err != nil {
		t.Fatal(err)
	}
}

func readResponse(t *testing.T, stream pluginv1.TransportPlugin_ForwardClient) (*pluginv1.ForwardResponseStart, []byte, *pluginv1.ForwardResponseEnd, *pluginv1.ForwardResponseError) {
	t.Helper()
	var start *pluginv1.ForwardResponseStart
	var body bytes.Buffer
	for {
		frame, err := stream.Recv()
		if err != nil {
			t.Fatalf("receive response: %v", err)
		}
		switch value := frame.Frame.(type) {
		case *pluginv1.ForwardResponse_Start:
			if start != nil {
				t.Fatal("duplicate response start")
			}
			start = value.Start
		case *pluginv1.ForwardResponse_BodyChunk:
			if start == nil {
				t.Fatal("body preceded response start")
			}
			_, _ = body.Write(value.BodyChunk)
		case *pluginv1.ForwardResponse_End:
			return start, body.Bytes(), value.End, nil
		case *pluginv1.ForwardResponse_Error:
			return start, body.Bytes(), nil, value.Error
		default:
			t.Fatal("unknown response frame")
		}
	}
}

func TestStreamingPreservesLargeBodiesAndMetadata(t *testing.T) {
	payload := bytes.Repeat([]byte("streaming\x00data\xff"), 400000)
	firstRead := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != "api.example.test" || r.URL.RawQuery != "a=1&a=2" || r.Header.Get("Authorization") != "Bearer test-secret" || len(r.Header.Values("X-Repeated")) != 2 || r.ContentLength != int64(len(payload)) {
			t.Error("request metadata changed")
		}
		first := make([]byte, 64*1024)
		if _, err := io.ReadFull(r.Body, first); err != nil {
			t.Error(err)
			return
		}
		close(firstRead)
		rest, err := io.ReadAll(r.Body)
		if err != nil || !bytes.Equal(append(first, rest...), payload) {
			t.Error("request body changed")
		}
		w.Header().Add("X-Repeated", "one")
		w.Header().Add("X-Repeated", "two")
		w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
		w.WriteHeader(http.StatusCreated)
		for at := 0; at < len(payload); at += 17003 {
			_, _ = w.Write(payload[at:min(at+17003, len(payload))])
			w.(http.Flusher).Flush()
		}
	}))
	defer upstream.Close()
	f := New()
	client := rpcClient(t, f)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	stream, err := client.Forward(ctx)
	if err != nil {
		t.Fatal(err)
	}
	first := startFrame(upstream.URL+"/stream?a=1&a=2", true, int64(len(payload)))
	first.GetStart().Host = "api.example.test"
	first.GetStart().Headers = map[string]*pluginv1.HeaderValues{
		"Authorization": {Values: []string{"Bearer test-secret"}},
		"X-Repeated":    {Values: []string{"a", "b"}},
	}
	if err := stream.Send(first); err != nil {
		t.Fatal(err)
	}
	if err := stream.Send(bodyFrame(payload[:64*1024])); err != nil {
		t.Fatal(err)
	}
	select {
	case <-firstRead:
	case <-ctx.Done():
		t.Fatal("request was buffered instead of streamed")
	}
	sent := make(chan error, 1)
	go func() {
		for at := 64 * 1024; at < len(payload); at += 32768 {
			if err := stream.Send(bodyFrame(payload[at:min(at+32768, len(payload))])); err != nil {
				sent <- err
				return
			}
		}
		if err := stream.Send(endFrame()); err != nil {
			sent <- err
			return
		}
		sent <- stream.CloseSend()
	}()
	start, body, end, problem := readResponse(t, stream)
	if err := <-sent; err != nil {
		t.Fatal(err)
	}
	if problem != nil || start.StatusCode != http.StatusCreated || start.ContentLength != int64(len(payload)) || len(start.Headers["X-Repeated"].Values) != 2 || !bytes.Equal(body, payload) || end.BytesReceived != int64(len(payload)) {
		t.Fatalf("response mismatch: start=%v end=%v error=%v bytes=%d", start, end, problem, len(body))
	}
}

func TestEarlyResponseDoesNotWaitForUpload(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := http.NewResponseController(w).EnableFullDuplex(); err != nil {
			t.Error(err)
		}
		first := make([]byte, 3)
		if _, err := io.ReadFull(r.Body, first); err != nil {
			return
		}
		w.WriteHeader(http.StatusRequestEntityTooLarge)
		_, _ = io.WriteString(w, "upload rejected")
		w.(http.Flusher).Flush()
	}))
	defer upstream.Close()
	client := rpcClient(t, New())
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	stream, err := client.Forward(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.Send(startFrame(upstream.URL, true, -1)); err != nil {
		t.Fatal(err)
	}
	if err := stream.Send(bodyFrame([]byte("abc"))); err != nil {
		t.Fatal(err)
	}
	start, body, end, problem := readResponse(t, stream)
	if problem != nil || start.StatusCode != 413 || string(body) != "upload rejected" || end == nil {
		t.Fatalf("early response failed: %v %q %v %v", start, body, end, problem)
	}
}

func TestRawCompressedResponseAndNoRedirects(t *testing.T) {
	var packed bytes.Buffer
	zip := gzip.NewWriter(&packed)
	_, _ = zip.Write([]byte("preserve compressed bytes"))
	_ = zip.Close()
	var redirected atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/compressed":
			if r.Header.Get("Accept-Encoding") != "" {
				t.Error("compression header injected")
			}
			w.Header().Set("Content-Encoding", "gzip")
			_, _ = w.Write(packed.Bytes())
		case "/redirect":
			w.Header().Set("Location", "/destination")
			w.WriteHeader(http.StatusTemporaryRedirect)
		case "/destination":
			redirected.Add(1)
		}
	}))
	defer upstream.Close()
	client := rpcClient(t, New())
	for _, path := range []string{"/compressed", "/redirect"} {
		t.Run(path, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			stream, err := client.Forward(ctx)
			if err != nil {
				t.Fatal(err)
			}
			first := startFrame(upstream.URL+path, false, 0)
			first.GetStart().Headers = map[string]*pluginv1.HeaderValues{"Authorization": {Values: []string{"Bearer secret"}}}
			sendEmpty(t, stream, first)
			start, body, _, problem := readResponse(t, stream)
			if problem != nil {
				t.Fatal(problem)
			}
			if path == "/compressed" && (!bytes.Equal(body, packed.Bytes()) || start.Headers["Content-Encoding"].Values[0] != "gzip") {
				t.Fatal("response decompressed")
			}
			if path == "/redirect" && (start.StatusCode != 307 || redirected.Load() != 0) {
				t.Fatal("redirect followed")
			}
		})
	}
}

func TestCancellationAndCloseInterruptHTTP(t *testing.T) {
	for _, closeForwarder := range []bool{false, true} {
		t.Run(fmt.Sprint(closeForwarder), func(t *testing.T) {
			entered, canceled := make(chan struct{}), make(chan struct{})
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { close(entered); <-r.Context().Done(); close(canceled) }))
			defer upstream.Close()
			f := New()
			client := rpcClient(t, f)
			ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
			defer cancel()
			stream, err := client.Forward(ctx)
			if err != nil {
				t.Fatal(err)
			}
			sendEmpty(t, stream, startFrame(upstream.URL, false, 0))
			select {
			case <-entered:
			case <-ctx.Done():
				t.Fatal("upstream not reached")
			}
			if closeForwarder {
				f.Close()
			} else {
				cancel()
			}
			frame, recvErr := stream.Recv()
			if !closeForwarder && recvErr == nil {
				t.Fatal("canceled RPC succeeded")
			}
			if closeForwarder && (recvErr != nil || frame.GetError() == nil || !frame.GetError().RequestSent || frame.GetError().Code != "CANCELED") {
				t.Fatalf("close lost attempted-request state: %v %v", frame, recvErr)
			}
			select {
			case <-canceled:
			case <-time.After(2 * time.Second):
				t.Fatal("upstream was not canceled")
			}
		})
	}
}

type fakeStream struct {
	grpc.ServerStream
	ctx       context.Context
	frames    []*pluginv1.ForwardRequest
	responses []*pluginv1.ForwardResponse
}

func (s *fakeStream) Context() context.Context { return s.ctx }
func (s *fakeStream) Recv() (*pluginv1.ForwardRequest, error) {
	if len(s.frames) == 0 {
		return nil, io.EOF
	}
	frame := s.frames[0]
	s.frames = s.frames[1:]
	return frame, nil
}
func (s *fakeStream) Send(frame *pluginv1.ForwardResponse) error {
	s.responses = append(s.responses, frame)
	return nil
}

func TestInvalidFramesAreNotSentAndErrorsAreSanitized(t *testing.T) {
	var requests atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests.Add(1) }))
	defer upstream.Close()
	valid := func() *pluginv1.ForwardRequest { return startFrame(upstream.URL, false, 0) }
	badURL := valid()
	badURL.GetStart().Url = "file:///secret-token"
	badHeader := valid()
	badHeader.GetStart().Headers = map[string]*pluginv1.HeaderValues{"Authorization": {Values: []string{"secret\r\ntoken"}}}
	badProxy := valid()
	badProxy.GetStart().ProxyUrl = "ftp://secret:password@localhost/"
	badLength := valid()
	badLength.GetStart().ContentLength = 5
	badMethod := valid()
	badMethod.GetStart().Method = ""
	badHost := valid()
	badHost.GetStart().Url = "http://:8080/"
	cases := map[string][]*pluginv1.ForwardRequest{
		"empty": nil, "body first": {bodyFrame([]byte("secret-body"))}, "empty frame": {{}},
		"duplicate start": {valid(), valid()}, "body without flag": {valid(), bodyFrame(nil)},
		"false end":   {valid(), {Frame: &pluginv1.ForwardRequest_BodyEnd{BodyEnd: false}}},
		"missing end": {valid()}, "trailing": {valid(), endFrame(), bodyFrame([]byte("secret-body"))},
		"url": {badURL}, "header": {badHeader}, "proxy": {badProxy}, "length": {badLength}, "method": {badMethod}, "host": {badHost},
	}
	for name, frames := range cases {
		t.Run(name, func(t *testing.T) {
			f := New()
			defer f.Close()
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			stream := &fakeStream{ctx: ctx, frames: frames}
			if err := f.Forward(stream); err != nil {
				t.Fatal(err)
			}
			if len(stream.responses) != 1 || stream.responses[0].GetError() == nil {
				t.Fatalf("expected error frame: %v", stream.responses)
			}
			problem := stream.responses[0].GetError()
			if problem.RequestSent || strings.Contains(problem.Message, "secret") || strings.Contains(problem.Message, upstream.URL) {
				t.Fatalf("unsafe error: %v", problem)
			}
		})
	}
	if requests.Load() != 0 {
		t.Fatal("invalid request reached upstream")
	}
}

func TestAttemptedRequestErrorsAlwaysSetRequestSent(t *testing.T) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	target := "http://" + lis.Addr().String() + "/secret-token"
	_ = lis.Close()
	f := New()
	defer f.Close()
	stream := &fakeStream{ctx: context.Background(), frames: []*pluginv1.ForwardRequest{startFrame(target, false, 0), endFrame()}}
	if err := f.Forward(stream); err != nil {
		t.Fatal(err)
	}
	problem := stream.responses[0].GetError()
	if problem == nil || !problem.RequestSent || strings.Contains(problem.Message, "secret") || strings.Contains(problem.Message, target) {
		t.Fatalf("unsafe attempt error: %v", problem)
	}
}

func TestMalformedUploadAfterAttemptIsNotReplayable(t *testing.T) {
	entered := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { close(entered); _, _ = io.Copy(io.Discard, r.Body) }))
	defer upstream.Close()
	client := rpcClient(t, New())
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	stream, err := client.Forward(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.Send(startFrame(upstream.URL, true, -1)); err != nil {
		t.Fatal(err)
	}
	if err := stream.Send(bodyFrame([]byte("first"))); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("not attempted")
	}
	if err := stream.Send(startFrame(upstream.URL, true, -1)); err != nil {
		t.Fatal(err)
	}
	_, _, _, problem := readResponse(t, stream)
	if problem == nil || !problem.RequestSent || problem.Code != "INVALID_REQUEST" {
		t.Fatalf("bad upload error: %v", problem)
	}
}

func TestHTTPProxyAndSOCKSProxies(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "proxied") }))
	defer upstream.Close()
	var proxyCalls atomic.Int32
	httpProxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxyCalls.Add(1)
		if r.Header.Get("Proxy-Authorization") != "Basic dXNlcjpwYXNz" {
			t.Error("proxy authentication missing")
		}
		out := r.Clone(r.Context())
		out.RequestURI = ""
		out.Header.Del("Proxy-Authorization")
		response, err := http.DefaultTransport.RoundTrip(out)
		if err != nil {
			http.Error(w, "proxy failed", 502)
			return
		}
		defer response.Body.Close()
		w.WriteHeader(response.StatusCode)
		_, _ = io.Copy(w, response.Body)
	}))
	defer httpProxy.Close()
	socksAddress, socksCalls := socksProxy(t, false)
	authSocksAddress, authSocksCalls := socksProxy(t, true)
	proxyURL, _ := url.Parse(httpProxy.URL)
	proxyURL.User = url.UserPassword("user", "pass")
	for _, proxyURL := range []string{proxyURL.String(), "socks5://" + socksAddress, "socks5h://" + socksAddress, "socks5h://user:pass@" + authSocksAddress} {
		t.Run(strings.Split(proxyURL, ":")[0], func(t *testing.T) {
			f := New()
			defer f.Close()
			first := startFrame(upstream.URL, false, 0)
			first.GetStart().ProxyUrl = proxyURL
			stream := &fakeStream{ctx: context.Background(), frames: []*pluginv1.ForwardRequest{first, endFrame()}}
			if err := f.Forward(stream); err != nil {
				t.Fatal(err)
			}
			var body []byte
			for _, frame := range stream.responses {
				if problem := frame.GetError(); problem != nil {
					t.Fatal(problem)
				}
				body = append(body, frame.GetBodyChunk()...)
			}
			if string(body) != "proxied" {
				t.Fatalf("proxy response: %q", body)
			}
		})
	}
	if proxyCalls.Load() != 1 || socksCalls.Load() != 2 || authSocksCalls.Load() != 1 {
		t.Fatal("proxy was bypassed")
	}
}

func socksProxy(t *testing.T, authenticate bool) (string, *atomic.Int32) {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lis.Close() })
	calls := new(atomic.Int32)
	go func() {
		for {
			conn, err := lis.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
				header := make([]byte, 2)
				if _, err := io.ReadFull(conn, header); err != nil {
					return
				}
				methods := make([]byte, int(header[1]))
				if _, err := io.ReadFull(conn, methods); err != nil {
					return
				}
				if authenticate {
					_, _ = conn.Write([]byte{5, 2})
					auth := make([]byte, 2)
					if _, err := io.ReadFull(conn, auth); err != nil {
						return
					}
					user := make([]byte, int(auth[1]))
					if _, err := io.ReadFull(conn, user); err != nil {
						return
					}
					length := make([]byte, 1)
					if _, err := io.ReadFull(conn, length); err != nil {
						return
					}
					password := make([]byte, int(length[0]))
					if _, err := io.ReadFull(conn, password); err != nil {
						return
					}
					if auth[0] != 1 || string(user) != "user" || string(password) != "pass" {
						t.Error("invalid SOCKS authentication")
						return
					}
					_, _ = conn.Write([]byte{1, 0})
				} else {
					_, _ = conn.Write([]byte{5, 0})
				}
				request := make([]byte, 4)
				if _, err := io.ReadFull(conn, request); err != nil {
					return
				}
				var host string
				switch request[3] {
				case 1:
					ip := make([]byte, 4)
					if _, err := io.ReadFull(conn, ip); err != nil {
						return
					}
					host = net.IP(ip).String()
				case 4:
					ip := make([]byte, 16)
					if _, err := io.ReadFull(conn, ip); err != nil {
						return
					}
					host = net.IP(ip).String()
				case 3:
					length := make([]byte, 1)
					if _, err := io.ReadFull(conn, length); err != nil {
						return
					}
					domain := make([]byte, int(length[0]))
					if _, err := io.ReadFull(conn, domain); err != nil {
						return
					}
					host = string(domain)
				default:
					return
				}
				port := make([]byte, 2)
				if _, err := io.ReadFull(conn, port); err != nil {
					return
				}
				upstream, err := net.Dial("tcp", net.JoinHostPort(host, strconv.Itoa(int(binary.BigEndian.Uint16(port)))))
				if err != nil {
					return
				}
				defer upstream.Close()
				calls.Add(1)
				_, _ = conn.Write([]byte{5, 0, 0, 1, 127, 0, 0, 1, 0, 0})
				done := make(chan struct{})
				go func() { _, _ = io.Copy(upstream, conn); close(done) }()
				_, _ = io.Copy(conn, upstream)
				_ = conn.Close()
				<-done
			}()
		}
	}()
	return lis.Addr().String(), calls
}

func TestCacheIsBoundedAndCloseRejectsNewRequests(t *testing.T) {
	f := New()
	for i := 0; i < maxCachedClients+20; i++ {
		client, problem := f.client(fmt.Sprintf("http://user:secret@127.0.0.1:%d", 10000+i))
		if problem != nil || client == nil {
			t.Fatal(problem)
		}
		transport := client.Transport.(*http.Transport)
		if !transport.DisableCompression || !transport.DisableKeepAlives || !transport.Protocols.HTTP1() || transport.Protocols.HTTP2() {
			t.Fatal("unsafe retry/compression settings")
		}
	}
	if len(f.clients) != maxCachedClients || f.lru.Len() != maxCachedClients {
		t.Fatal("client cache grew without a bound")
	}
	f.Close()
	f.Close()
	if len(f.clients) != 0 || f.lru.Len() != 0 {
		t.Fatal("cache not closed")
	}
	stream := &fakeStream{ctx: context.Background()}
	if err := f.Forward(stream); err != nil {
		t.Fatal(err)
	}
	if problem := stream.responses[0].GetError(); problem == nil || problem.RequestSent || problem.Code != "CLOSED" {
		t.Fatalf("bad closed error: %v", problem)
	}
}

func TestStandardTLSVerificationIsEnforced(t *testing.T) {
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("untrusted TLS request reached handler") }))
	defer upstream.Close()
	f := New()
	defer f.Close()
	stream := &fakeStream{ctx: context.Background(), frames: []*pluginv1.ForwardRequest{startFrame(upstream.URL, false, 0), endFrame()}}
	if err := f.Forward(stream); err != nil {
		t.Fatal(err)
	}
	if problem := stream.responses[0].GetError(); problem == nil || !problem.RequestSent || problem.Code != "UPSTREAM_ERROR" {
		t.Fatalf("untrusted TLS accepted: %v", problem)
	}
}

func TestHTTPSDirectAndAuthenticatedConnectProxies(t *testing.T) {
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer origin-secret" || r.Header.Get("Proxy-Authorization") != "" {
			t.Error("origin credentials changed or proxy credentials leaked")
		}
		_, _ = io.WriteString(w, "secure response")
	}))
	defer upstream.Close()
	var calls atomic.Int32
	connectHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "CONNECT" || r.Header.Get("Proxy-Authorization") != "Basic dXNlcjpwYXNz" {
			t.Error("invalid authenticated CONNECT")
			w.WriteHeader(407)
			return
		}
		remote, err := net.Dial("tcp", r.Host)
		if err != nil {
			w.WriteHeader(502)
			return
		}
		defer remote.Close()
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()
		calls.Add(1)
		_, _ = io.WriteString(conn, "HTTP/1.1 200 Connection Established\r\n\r\n")
		done := make(chan struct{})
		go func() { _, _ = io.Copy(remote, conn); close(done) }()
		_, _ = io.Copy(conn, remote)
		_ = conn.Close()
		<-done
	})
	httpProxy := httptest.NewServer(connectHandler)
	defer httpProxy.Close()
	httpsProxy := httptest.NewTLSServer(connectHandler)
	defer httpsProxy.Close()
	roots := x509.NewCertPool()
	roots.AddCert(upstream.Certificate())
	roots.AddCert(httpsProxy.Certificate())
	for _, rawProxy := range []string{"", httpProxy.URL, httpsProxy.URL} {
		proxyURL := rawProxy
		if rawProxy != "" {
			u, _ := url.Parse(rawProxy)
			u.User = url.UserPassword("user", "pass")
			proxyURL = u.String()
		}
		f := New()
		client, problem := f.client(proxyURL)
		if problem != nil {
			t.Fatal(problem)
		}
		client.Transport.(*http.Transport).TLSClientConfig = &tls.Config{RootCAs: roots}
		first := startFrame(upstream.URL, false, 0)
		first.GetStart().ProxyUrl = proxyURL
		first.GetStart().Headers = map[string]*pluginv1.HeaderValues{"Authorization": {Values: []string{"Bearer origin-secret"}}}
		stream := &fakeStream{ctx: context.Background(), frames: []*pluginv1.ForwardRequest{first, endFrame()}}
		if err := f.Forward(stream); err != nil {
			t.Fatal(err)
		}
		var body []byte
		for _, frame := range stream.responses {
			if problem := frame.GetError(); problem != nil {
				t.Fatal(problem)
			}
			body = append(body, frame.GetBodyChunk()...)
		}
		f.Close()
		if string(body) != "secure response" {
			t.Fatalf("HTTPS response: %q", body)
		}
	}
	if calls.Load() != 2 {
		t.Fatal("CONNECT proxy was bypassed")
	}
}

func TestHTTPFailuresAreNeverRetried(t *testing.T) {
	var attempts atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		_ = conn.Close()
	}))
	defer upstream.Close()
	f := New()
	defer f.Close()
	for i := 0; i < 2; i++ {
		first := startFrame(upstream.URL, false, 0)
		first.GetStart().Method = http.MethodGet
		stream := &fakeStream{ctx: context.Background(), frames: []*pluginv1.ForwardRequest{first, endFrame()}}
		if err := f.Forward(stream); err != nil {
			t.Fatal(err)
		}
		if problem := stream.responses[0].GetError(); problem == nil || !problem.RequestSent {
			t.Fatalf("missing request_sent: %v", problem)
		}
	}
	if attempts.Load() != 2 {
		t.Fatalf("HTTP request was retried: %d attempts", attempts.Load())
	}
}

func TestDeclaredBodyLengthMismatchFailsAfterAttempt(t *testing.T) {
	for _, length := range []int64{1, 10} {
		t.Run(fmt.Sprint(length), func(t *testing.T) {
			entered := make(chan struct{})
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { close(entered); _, _ = io.Copy(io.Discard, r.Body) }))
			defer upstream.Close()
			client := rpcClient(t, New())
			ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
			defer cancel()
			stream, err := client.Forward(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if err := stream.Send(startFrame(upstream.URL, true, length)); err != nil {
				t.Fatal(err)
			}
			// Wait for headers so length validation is demonstrably post-attempt.
			select {
			case <-entered:
			case <-ctx.Done():
				t.Fatal("request not attempted")
			}
			if err := stream.Send(bodyFrame([]byte("abc"))); err != nil {
				t.Fatal(err)
			}
			if err := stream.Send(endFrame()); err != nil {
				t.Fatal(err)
			}
			_ = stream.CloseSend()
			_, _, _, problem := readResponse(t, stream)
			if problem == nil || !problem.RequestSent || problem.Code != "INVALID_REQUEST" {
				t.Fatalf("body length error: %v", problem)
			}
		})
	}
}

func TestCanceledSOCKSHandshakeClosesConnection(t *testing.T) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer lis.Close()
	accepted, closed := make(chan struct{}), make(chan struct{})
	go func() {
		conn, err := lis.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		close(accepted)
		_, _ = io.Copy(io.Discard, conn)
		close(closed)
	}()
	client := rpcClient(t, New())
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	stream, err := client.Forward(ctx)
	if err != nil {
		t.Fatal(err)
	}
	first := startFrame("http://127.0.0.1:1/", false, 0)
	first.GetStart().ProxyUrl = "socks5h://" + lis.Addr().String()
	sendEmpty(t, stream, first)
	select {
	case <-accepted:
	case <-ctx.Done():
		t.Fatal("SOCKS dial did not start")
	}
	cancel()
	if _, err := stream.Recv(); err == nil {
		t.Fatal("canceled proxy RPC succeeded")
	}
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("SOCKS connection leaked after cancellation")
	}
}

func TestIncompleteResponseEndsWithSentError(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "10")
		_, _ = io.WriteString(w, "abc")
	}))
	defer upstream.Close()
	client := rpcClient(t, New())
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	stream, err := client.Forward(ctx)
	if err != nil {
		t.Fatal(err)
	}
	sendEmpty(t, stream, startFrame(upstream.URL, false, 0))
	start, body, end, problem := readResponse(t, stream)
	if start == nil || string(body) != "abc" || end != nil || problem == nil || !problem.RequestSent || problem.Code != "UPSTREAM_READ_ERROR" {
		t.Fatalf("partial response failure: %v %q %v %v", start, body, end, problem)
	}
}
