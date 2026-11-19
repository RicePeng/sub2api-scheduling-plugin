package tests

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/go-hclog"
	hcplugin "github.com/hashicorp/go-plugin"
	"google.golang.org/grpc"

	"sub2api.local/cache-manager/internal/config"
	pluginv1 "sub2api.local/cache-manager/internal/hostapi"
)

type hostKV struct {
	pluginv1.UnimplementedHostServiceServer
	mu    sync.Mutex
	value []byte
}

func (h *hostKV) KVGet(_ context.Context, r *pluginv1.KVGetRequest) (*pluginv1.KVGetResponse, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return &pluginv1.KVGetResponse{Found: len(h.value) > 0, Value: append([]byte{}, h.value...)}, nil
}
func (h *hostKV) KVSet(_ context.Context, r *pluginv1.KVSetRequest) (*pluginv1.KVSetResponse, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.value = append([]byte{}, r.Value...)
	return &pluginv1.KVSetResponse{}, nil
}

func TestRealPluginProcessHandshakeForwardAndDurableManagement(t *testing.T) {
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	goName := "go"
	if runtime.GOOS == "windows" {
		goName = "go.exe"
	}
	goTool := filepath.Join(runtime.GOROOT(), "bin", goName)
	binary := filepath.Join(t.TempDir(), "manager")
	if runtime.GOOS == "windows" {
		binary += ".exe"
	}
	command := exec.Command(goTool, "build", "-o", binary, "./cmd/plugin")
	command.Dir = root
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, output)
	}
	client := hcplugin.NewClient(&hcplugin.ClientConfig{
		HandshakeConfig: pluginv1.HandshakeConfig, Plugins: pluginv1.ClientPluginMap(),
		Cmd: exec.Command(binary), AllowedProtocols: []hcplugin.Protocol{hcplugin.ProtocolGRPC},
		StartTimeout: 15 * time.Second, Logger: hclog.NewNullLogger(),
	})
	defer client.Kill()
	rpcClient, err := client.Client()
	if err != nil {
		t.Fatal(err)
	}
	object, err := rpcClient.Dispense(pluginv1.TransportPluginName)
	if err != nil {
		t.Fatal(err)
	}
	transport := object.(*pluginv1.TransportClient)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	info, err := transport.GetInfo(ctx, &pluginv1.GetInfoRequest{})
	if err != nil || info.PluginId != "local.cache-rate-manager" || info.ProtocolVersion != 1 {
		t.Fatalf("%+v %v", info, err)
	}
	kv := &hostKV{}
	serviceID := transport.Broker.NextId()
	go transport.Broker.AcceptAndServe(serviceID, func(options []grpc.ServerOption) *grpc.Server {
		server := grpc.NewServer(options...)
		pluginv1.RegisterHostServiceServer(server, kv)
		return server
	})
	ready, err := transport.InitHostServices(ctx, &pluginv1.InitHostServicesRequest{HostServiceId: serviceID, HostServiceApiVersion: 2})
	if err != nil || !ready.Ready {
		t.Fatalf("%+v %v", ready, err)
	}
	health, err := transport.Health(ctx, &pluginv1.HealthRequest{})
	var state struct {
		Hostname string `json:"hostname"`
	}
	if err != nil || !health.Healthy || json.Unmarshal([]byte(health.StatusJson), &state) != nil || state.Hostname == "" {
		t.Fatalf("health=%+v %v", health, err)
	}
	// The real subprocess must forward upstream bytes rather than synthesizing output.
	payload := []byte("native plugin streaming request")
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Test") != "forwarded" {
			t.Error("request header lost")
		}
		w.Header().Set("X-Upstream", "real")
		_, _ = io.Copy(w, r.Body)
	}))
	defer upstream.Close()
	stream, err := transport.Forward(ctx)
	if err != nil {
		t.Fatal(err)
	}
	err = stream.Send(&pluginv1.ForwardRequest{Frame: &pluginv1.ForwardRequest_Start{Start: &pluginv1.ForwardRequestStart{
		Method: "POST", Url: upstream.URL, AccountId: 7, Platform: "openai", AccountType: "oauth",
		HasBody: true, ContentLength: int64(len(payload)), Headers: map[string]*pluginv1.HeaderValues{
			"X-Test": {Values: []string{"forwarded"}},
		},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	_ = stream.Send(&pluginv1.ForwardRequest{Frame: &pluginv1.ForwardRequest_BodyChunk{BodyChunk: payload}})
	_ = stream.Send(&pluginv1.ForwardRequest{Frame: &pluginv1.ForwardRequest_BodyEnd{BodyEnd: true}})
	_ = stream.CloseSend()
	var got bytes.Buffer
	gotEnd := false
	for {
		frame, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if frame.GetError() != nil {
			t.Fatal(frame.GetError().Message)
		}
		got.Write(frame.GetBodyChunk())
		if frame.GetEnd() != nil {
			gotEnd = true
		}
	}
	if !gotEnd || !bytes.Equal(got.Bytes(), payload) {
		t.Fatalf("forward failed: %q %v", got.String(), gotEnd)
	}

	var mu sync.Mutex
	schedulable, priority, writes, usageReads := true, 5, 0, 0
	admin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") != "fixture-key" {
			t.Error("management key missing")
		}
		mu.Lock()
		defer mu.Unlock()
		data := any(nil)
		switch {
		case r.URL.Path == "/api/v1/admin/accounts/7/schedulable":
			var input struct {
				Schedulable bool `json:"schedulable"`
			}
			_ = json.NewDecoder(r.Body).Decode(&input)
			schedulable, writes = input.Schedulable, writes+1
			data = map[string]any{"id": 7}
		case r.URL.Path == "/api/v1/admin/accounts/7":
			if r.Method == http.MethodPut {
				var input struct {
					Priority int `json:"priority"`
				}
				_ = json.NewDecoder(r.Body).Decode(&input)
				priority, writes = input.Priority, writes+1
			}
			data = map[string]any{"id": 7, "name": "fixture", "platform": "openai", "type": "apikey", "status": "active", "schedulable": schedulable, "priority": priority, "rate_multiplier": 2}
		case r.URL.Path == "/api/v1/admin/usage":
			usageReads++
			data = map[string]any{"items": []any{}}
		case r.URL.Path == "/api/v1/admin/accounts/upstream-billing-rates":
			data = map[string]any{"items": []any{}}
		default:
			t.Errorf("unexpected endpoint %s", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": data})
	}))
	defer admin.Close()
	c := config.Defaults()
	c.AdminURL, c.AdminAPIKey, c.AccountIDs = admin.URL+"/api/v1", "fixture-key", []int64{7}
	c.AutomationEnabled, c.ExecutorHostname = true, state.Hostname
	c.AccountSettings = []config.AccountSetting{{AccountID: 7, Group: "temporary", RateMode: "upstream"}}
	applied, err := transport.ApplyConfig(ctx, &pluginv1.ApplyConfigRequest{ConfigJson: c.JSON()})
	if err != nil || !applied.Applied {
		t.Fatalf("%+v %v", applied, err)
	}
	waitFor := func(expected bool, expectedPriority int) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			mu.Lock()
			current, currentPriority := schedulable, priority
			mu.Unlock()
			if current == expected && currentPriority == expectedPriority {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatal("management state did not converge")
	}
	waitFor(false, 99)
	readCycle := func(previous string) string {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			h, readErr := transport.Health(ctx, &pluginv1.HealthRequest{})
			var poll struct {
				LastPoll string `json:"last_poll"`
				Polling  bool   `json:"polling"`
			}
			if readErr == nil && json.Unmarshal([]byte(h.StatusJson), &poll) == nil && !poll.Polling && poll.LastPoll != "" && poll.LastPoll != previous {
				return poll.LastPoll
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatal("manual collection did not complete")
		return ""
	}
	last := readCycle("")
	mu.Lock()
	beforeReads := usageReads
	mu.Unlock()
	manual, err := transport.TestConfig(ctx, &pluginv1.TestConfigRequest{ConfigJson: c.JSON()})
	if err != nil || !manual.Success {
		t.Fatalf("manual request rejected: %+v %v", manual, err)
	}
	readCycle(last)
	mu.Lock()
	afterReads := usageReads
	mu.Unlock()
	if afterReads <= beforeReads {
		t.Fatal("manual request only read a cached health snapshot")
	}
	c.AutomationEnabled = false
	applied, err = transport.ApplyConfig(ctx, &pluginv1.ApplyConfigRequest{ConfigJson: c.JSON()})
	if err != nil || !applied.Applied {
		t.Fatalf("%+v %v", applied, err)
	}
	waitFor(true, 5)
	mu.Lock()
	writeCount := writes
	mu.Unlock()
	if writeCount != 4 {
		t.Fatalf("expected temporary pause, priority99 and both restores, got %d writes", writeCount)
	}
	health, err = transport.Health(ctx, &pluginv1.HealthRequest{})
	if err != nil || !health.Healthy || bytes.Contains([]byte(health.StatusJson), []byte("fixture-key")) {
		t.Fatal("unsafe status or broken health")
	}
	_ = os.Remove(binary) // process may still hold executable on Windows; TempDir cleanup follows Kill
}
