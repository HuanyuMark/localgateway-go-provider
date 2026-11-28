// Package provider 是 LocalGateway provider 端的最小运行时。
//
// 业务路由、OpenAPI 文档与内建端点的注册全部交给 Huma：
//
//	mux := http.NewServeMux()
//	api := humago.New(mux, huma.DefaultConfig("my-service", "1.0.0"))
//	huma.Register(api, huma.Operation{
//	    Method: http.MethodGet, Path: "/hello", OperationID: "hello",
//	}, helloHandler)
//
//	p, err := provider.New().
//	    Name("my-service").
//	    Version("1.0.0").
//	    API(api).
//	    Serve(provider.ServeNetHTTP(mux)).
//	    Build()
//	if err != nil { ... }
//	if err := p.Run(ctx); err != nil { ... }
//
// 库只额外提供 GET /health 与 POST /_localgateway/events 两个内建端点：
// 若用户在 API 上已经注册了同路径同方法，则保留用户的定义。注册时把
// api.OpenAPI() 导出的文档作为 apiInfo 上报网关。
//
// 跨域默认放行所有来源：注册时携带 {"allowAll": true}，浏览器无需额外配置即可
// 调用服务。需要收窄时用 Builder.CORS 指定来源，需要完全关闭时用 Builder.NoCORS。
package provider

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net"
	"strings"
	"sync"
	"time"

	localgateway "github.com/HuanyuMark/localgateway-go/client"
)

// 默认配置值。
const (
	// DefaultProbePath 是内建健康检查端点。
	DefaultProbePath = "/health"
	// DefaultNotifyPath 是接收网关重启预通知的端点。
	DefaultNotifyPath = "/_localgateway/events"
	// DefaultProbeTimeoutMs 与网关默认探活超时一致。
	DefaultProbeTimeoutMs = 2000
	// DefaultRegisterRetry 是注册失败后的最长重试窗口。
	DefaultRegisterRetry = 30 * time.Second
)

// 配置与生命周期错误。调用方可用 errors.Is 判定。
var (
	// ErrInvalidConfig 表示 Builder 配置不合法。
	ErrInvalidConfig = errors.New("provider: invalid config")
	// ErrAlreadyStarted 表示同一个 Provider 被重复启动。
	ErrAlreadyStarted = errors.New("provider: already started")
	// ErrClosed 表示 Provider 已经停止或启动失败，不能再次使用。
	ErrClosed = errors.New("provider: provider is closed")
)

// providerConfig 是 Builder 归一化后的私有配置。
type providerConfig struct {
	Name                 string
	Version              string
	Port                 int
	ProbePath            string
	ProbeTimeoutMs       int
	ProjectRoot          string
	NotifyPath           string
	UnregisterOnShutdown bool
	RegisterRetry        time.Duration
	// CORS 随注册上报；默认 AllowAll，NoCORS 为 true 时不携带。
	CORS          *localgateway.Cors
	NoCORS        bool
	GatewayClient *localgateway.Client
}

// Provider 是一个已校验、可启动的 provider。
type Provider struct {
	cfg   providerConfig
	spec  map[string]any
	serve ServeFunc

	mu          sync.Mutex
	listener    net.Listener
	port        int
	client      *localgateway.Client
	started     bool
	closed      bool
	registered  bool
	serveCancel context.CancelFunc
	serveErr    chan error
	serveExited bool
}

// Port 返回实际监听端口；启动前为 0。端口为 0 启动后返回系统分配的端口。
func (p *Provider) Port() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.port
}

// normalizeProviderConfig 填充默认值并校验配置；所有问题会累积返回。
func normalizeProviderConfig(config providerConfig) (providerConfig, error) {
	var errs []error

	if strings.TrimSpace(config.Name) == "" {
		errs = append(errs, fmt.Errorf("%w: Name is required", ErrInvalidConfig))
	}
	if strings.TrimSpace(config.Version) == "" {
		errs = append(errs, fmt.Errorf("%w: Version is required", ErrInvalidConfig))
	}
	if config.Port < 0 || config.Port > 65535 {
		errs = append(errs, fmt.Errorf("%w: Port must be between 0 and 65535", ErrInvalidConfig))
	}
	if config.ProbePath == "" {
		config.ProbePath = DefaultProbePath
	}
	if config.NotifyPath == "" {
		config.NotifyPath = DefaultNotifyPath
	}
	if err := validateEndpointPath("ProbePath", config.ProbePath); err != nil {
		errs = append(errs, err)
	}
	if err := validateEndpointPath("NotifyPath", config.NotifyPath); err != nil {
		errs = append(errs, err)
	}
	if config.ProbePath == config.NotifyPath {
		errs = append(errs, fmt.Errorf("%w: ProbePath and NotifyPath must differ", ErrInvalidConfig))
	}
	if config.ProbeTimeoutMs < 0 || config.ProbeTimeoutMs > math.MaxInt32 {
		errs = append(errs, fmt.Errorf("%w: ProbeTimeoutMs must be between 0 and %d", ErrInvalidConfig, math.MaxInt32))
	} else if config.ProbeTimeoutMs == 0 {
		config.ProbeTimeoutMs = DefaultProbeTimeoutMs
	}
	if config.RegisterRetry < 0 {
		errs = append(errs, fmt.Errorf("%w: RegisterRetry must not be negative", ErrInvalidConfig))
	} else if config.RegisterRetry == 0 {
		config.RegisterRetry = DefaultRegisterRetry
	}
	if config.NoCORS {
		config.CORS = nil
	} else if config.CORS == nil {
		config.CORS = &localgateway.Cors{AllowAll: true}
	}

	return config, errors.Join(errs...)
}

func validateEndpointPath(field, path string) error {
	if !strings.HasPrefix(path, "/") {
		return fmt.Errorf("%w: %s must start with '/'", ErrInvalidConfig, field)
	}
	if path == "/" {
		return fmt.Errorf("%w: %s must not be the root path", ErrInvalidConfig, field)
	}
	if strings.HasSuffix(path, "/") {
		return fmt.Errorf("%w: %s must not end with '/'", ErrInvalidConfig, field)
	}
	if strings.ContainsAny(path, " \t\r\n?#") {
		return fmt.Errorf("%w: %s must not contain whitespace, '?' or '#'", ErrInvalidConfig, field)
	}
	return nil
}

func (p *Provider) gatewayClient() *localgateway.Client {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.client
}
