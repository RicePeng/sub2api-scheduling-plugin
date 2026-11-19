package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	hcplugin "github.com/hashicorp/go-plugin"
	"google.golang.org/grpc"

	"sub2api.local/cache-manager/internal/adminapi"
	"sub2api.local/cache-manager/internal/config"
	pluginv1 "sub2api.local/cache-manager/internal/hostapi"
	"sub2api.local/cache-manager/internal/monitor"
	"sub2api.local/cache-manager/internal/transport"
)

const ID = "local.cache-rate-manager"
const Capability = "openai.oauth.outbound_transport.v1"

type Server struct {
	pluginv1.UnimplementedTransportPluginServer
	version  string
	engine   *monitor.Engine
	forward  *transport.Forwarder
	cancel   context.CancelFunc
	mu       sync.Mutex
	broker   *hcplugin.GRPCBroker
	hostConn *grpc.ClientConn
}

func New(version, hostname string) *Server {
	ctx, cancel := context.WithCancel(context.Background())
	s := &Server{version: version, engine: monitor.New(version, hostname),
		forward: transport.New(), cancel: cancel}
	go s.engine.Run(ctx)
	return s
}

func (s *Server) Close() {
	s.cancel()
	s.forward.Close()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.hostConn != nil {
		_ = s.hostConn.Close()
	}
}

func (s *Server) SetHostBroker(broker *hcplugin.GRPCBroker) {
	s.mu.Lock()
	s.broker = broker
	s.mu.Unlock()
}

func (s *Server) GetInfo(context.Context, *pluginv1.GetInfoRequest) (*pluginv1.GetInfoResponse, error) {
	return &pluginv1.GetInfoResponse{PluginId: ID, PluginVersion: s.version,
		ProtocolVersion: pluginv1.ProtocolVersion, TransportApiVersion: pluginv1.TransportAPIVersion,
		Capabilities: []string{Capability}}, nil
}

func (s *Server) Health(context.Context, *pluginv1.HealthRequest) (*pluginv1.HealthResponse, error) {
	status, _ := json.Marshal(s.engine.Snapshot())
	// Management API failures must never disable an otherwise working transport.
	return &pluginv1.HealthResponse{Healthy: true, Message: "传输已就绪；管理状态见插件面板", StatusJson: string(status)}, nil
}

func (s *Server) ValidateConfig(_ context.Context, request *pluginv1.ValidateConfigRequest) (*pluginv1.ValidateConfigResponse, error) {
	c, err := config.Decode(request.ConfigJson)
	if err != nil {
		return &pluginv1.ValidateConfigResponse{Valid: false, Message: err.Error()}, nil
	}
	return &pluginv1.ValidateConfigResponse{Valid: true, NormalizedConfigJson: c.JSON()}, nil
}

func (s *Server) ApplyConfig(_ context.Context, request *pluginv1.ApplyConfigRequest) (*pluginv1.ApplyConfigResponse, error) {
	c, err := config.Decode(request.ConfigJson)
	if err == nil {
		err = s.engine.Apply(c)
	}
	if err != nil {
		return &pluginv1.ApplyConfigResponse{Applied: false, Message: err.Error()}, nil
	}
	return &pluginv1.ApplyConfigResponse{Applied: true, Message: "配置已应用；优先级与调度将在后台采集周期同步"}, nil
}

func (s *Server) TestConfig(ctx context.Context, request *pluginv1.TestConfigRequest) (*pluginv1.TestConfigResponse, error) {
	start := time.Now()
	c, err := config.Decode(request.ConfigJson)
	if err == nil && (c.AdminURL == "" || len(c.AccountIDs) == 0) {
		err = errors.New("请先保存管理连接和至少一个账号 ID")
	}
	if err == nil && c.AutomationEnabled {
		err = s.engine.RequestPoll()
		message := "已请求立即采集；本轮完成后重新开始自动请求倒计时"
		if err != nil {
			message = err.Error()
		}
		status, _ := json.Marshal(s.engine.Snapshot())
		return &pluginv1.TestConfigResponse{Success: err == nil, Message: message,
			LatencyMs: time.Since(start).Milliseconds(), StatusJson: string(status)}, nil
	}
	if err == nil {
		client := adminapi.New(c.AdminURL, c.AdminAPIKey, time.Duration(c.TimeoutSeconds)*time.Second)
		defer client.Close()
		account, readErr := client.Account(ctx, c.AccountIDs[0])
		err = readErr
		if err == nil && account.Type != "apikey" {
			err = errors.New("仅支持 API Key 账号，请检查添加的账号 ID")
		}
		if err == nil {
			window, usageErr := client.RecentUsage(ctx, account.ID, start)
			err = usageErr
			if account.RateMultiplier == nil {
				account.RateMultiplier = window.AccountRate
			}
		}
		if err == nil && c.UpstreamRatesEnabled {
			_, rateErr := client.Rates(ctx)
			err = rateErr
		}
	}
	message := "管理连接、首个账号和缓存读取权限正常；未执行任何管理动作"
	if err != nil {
		message = err.Error()
	}
	status, _ := json.Marshal(s.engine.Snapshot())
	return &pluginv1.TestConfigResponse{Success: err == nil, Message: message,
		LatencyMs: time.Since(start).Milliseconds(), StatusJson: string(status)}, nil
}

func (s *Server) Forward(stream pluginv1.TransportPlugin_ForwardServer) error {
	return s.forward.Forward(stream)
}

func (s *Server) InitHostServices(_ context.Context, request *pluginv1.InitHostServicesRequest) (*pluginv1.InitHostServicesResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.broker == nil || request.HostServiceId == 0 || request.HostServiceApiVersion < 1 {
		return &pluginv1.InitHostServicesResponse{Ready: false, Message: "宿主 KV 不可用；仅允许监控"}, nil
	}
	connection, err := s.broker.Dial(request.HostServiceId)
	if err != nil {
		return &pluginv1.InitHostServicesResponse{Ready: false, Message: "宿主 KV 连接失败；仅允许监控"}, nil
	}
	if s.hostConn != nil {
		_ = s.hostConn.Close()
	}
	s.hostConn = connection
	s.engine.SetStore(&hostStore{client: pluginv1.NewHostServiceClient(connection)})
	return &pluginv1.InitHostServicesResponse{Ready: true, Message: "管理记录存储已连接"}, nil
}

type hostStore struct{ client pluginv1.HostServiceClient }

func (h *hostStore) Get(ctx context.Context, key string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	result, err := h.client.KVGet(ctx, &pluginv1.KVGetRequest{Namespace: "manager", Key: key})
	if err != nil {
		return nil, fmt.Errorf("宿主状态读取失败")
	}
	if !result.Found {
		return nil, nil
	}
	return result.Value, nil
}
func (h *hostStore) Set(ctx context.Context, key string, value []byte) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_, err := h.client.KVSet(ctx, &pluginv1.KVSetRequest{Namespace: "manager", Key: key, Value: value})
	if err != nil {
		return errors.New("宿主状态保存失败")
	}
	return nil
}
