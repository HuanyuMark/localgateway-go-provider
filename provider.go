// Package provider 是 LocalGateway provider 端的最小运行时。
//
// 它把注册、探活、OpenAPI 上报、重启通知与优雅退出收进一个库：
//
//	p, err := provider.New(provider.Config{
//	    Name:    "my-service",
//	    Version: "1.0.0",
//	    Handler: mux,
//	})
//	if err != nil { ... }
//	if err := p.Run(ctx); err != nil { ... }
//
// 业务路由仍由调用方自己的 net/http handler（或通过 Serve 适配的其它框架）
// 提供；库只额外提供 GET /health 与 POST /_localgateway/events 两个内建端点。
package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net"
	"net/http"
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
	// ErrInvalidConfig 表示 Config 或 Route 不合法。
	ErrInvalidConfig = errors.New("provider: invalid config")
	// ErrRouteConflict 表示业务 handler 已占用内建路径。
	ErrRouteConflict = errors.New("provider: built-in endpoint conflicts with the business handler")
	// ErrAlreadyStarted 表示同一个 Provider 被重复启动。
	ErrAlreadyStarted = errors.New("provider: already started")
	// ErrClosed 表示 Provider 已经停止或启动失败，不能再次使用。
	ErrClosed = errors.New("provider: provider is closed")
)

// ServeFunc 是适配自定义 web 框架的入口。
//
// 实现只需在 listener 上服务最终 handler，并在 ctx 取消后停止并返回；
// handler 已经包含内建健康检查、通知端点与业务 Handler。返回 nil 表示正常退出。
type ServeFunc func(ctx context.Context, listener net.Listener, handler http.Handler) error

// Route 声明一条业务路由，仅用于自动组装 OpenAPI 文档。
//
// 业务路由本身仍由 Config.Handler 提供，库不会据此注册 handler。
type Route struct {
	// Method 是 HTTP 方法，大小写不敏感（内部规范化为大写）。
	Method string
	// Path 必须以 "/" 开头，可包含 OpenAPI 路径参数如 "/items/{id}"。
	Path string
	// OperationID 为空时不写入文档；非空时必须全局唯一。
	OperationID string
	// Summary 是可选的人类可读摘要。
	Summary string
	// RequestBody 是 application/json 请求体的 JSON Schema；nil 表示没有请求体。
	RequestBody any
	// Responses 把状态码映射到 OpenAPI Response Object。
	// 值可以是字符串（作为 description）或完整的 Response Object（map[string]any）；
	// 留空时生成 200/OK。
	Responses map[string]any
	// Tags 是可选的分组标签。
	Tags []string
}

// Config 是 Provider 的完整配置。
type Config struct {
	// Name 是注册的服务名，必填。
	Name string
	// Version 是 provider 版本，必填。
	Version string
	// Port 是监听端口；0 表示由系统分配临时端口。
	Port int
	// ProbePath 是内建健康检查路径，默认 /health。
	ProbePath string
	// ProbeTimeoutMs 是网关探活超时，0 使用 2000ms。
	ProbeTimeoutMs int
	// ProjectRoot 可选；非空时随注册信息上报。
	ProjectRoot string
	// NotifyPath 是重启通知端点，默认 /_localgateway/events。
	NotifyPath string
	// UnregisterOnShutdown 为真时在退出阶段 DELETE 注册。
	UnregisterOnShutdown bool
	// RegisterRetry 是注册失败后的重试窗口，0 使用 30s。
	RegisterRetry time.Duration
	// OpenAPI 是原始 OpenAPI 3.0/3.1 文档（map 或可 JSON 序列化的结构）；
	// 非 nil 时优先使用，只补齐 info 与探活路径。
	OpenAPI any
	// Routes 用于在 OpenAPI 为空时自动组装文档。
	Routes []Route
	// Handler 是业务 http.Handler；nil 表示只有内建端点。
	Handler http.Handler
	// HealthHandler 覆盖内建探活端点（GET ProbePath）的响应；非空时直接调用，
	// 由调用方决定状态码、Content-Type 与 body，并可读取 request context。
	// 为空时保持默认的 200 JSON：{"status":"ok","service":...,"version":...}。
	// 选用 http.HandlerFunc 而非返回 (int, any) 的回调，是为了让调用方完全
	// 控制响应格式，并与 Config.Handler 的 http.Handler 习惯一致。
	HealthHandler http.HandlerFunc
	// Serve 是自定义适配器；nil 时使用内置 net/http 服务。
	Serve ServeFunc

	// GatewayClient 可选：复用已创建的管理面客户端（测试或自定义 PortReader）。
	// 为空时在启动阶段用默认 Options 创建。
	GatewayClient *localgateway.Client
}

// Provider 是一个已校验、可启动的 provider。
type Provider struct {
	cfg     Config
	spec    map[string]any
	handler http.Handler
	serve   ServeFunc

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

// New 校验配置并组装最终 handler 与 OpenAPI 文档，但不监听端口。
func New(config Config) (*Provider, error) {
	normalized, err := normalizeConfig(config)
	if err != nil {
		return nil, err
	}
	routes, err := normalizeRoutes(normalized.Routes)
	if err != nil {
		return nil, err
	}
	normalized.Routes = routes

	spec, err := buildOpenAPI(normalized, routes)
	if err != nil {
		return nil, err
	}

	provider := &Provider{
		cfg:   normalized,
		spec:  spec,
		serve: normalized.Serve,
	}
	handler, err := provider.buildHandler()
	if err != nil {
		return nil, err
	}
	provider.handler = handler
	return provider, nil
}

// Port 返回实际监听端口；启动前为 0。端口为 0 启动后返回系统分配的端口。
func (p *Provider) Port() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.port
}

func normalizeConfig(config Config) (Config, error) {
	if strings.TrimSpace(config.Name) == "" {
		return Config{}, fmt.Errorf("%w: Name is required", ErrInvalidConfig)
	}
	if strings.TrimSpace(config.Version) == "" {
		return Config{}, fmt.Errorf("%w: Version is required", ErrInvalidConfig)
	}
	if config.Port < 0 || config.Port > 65535 {
		return Config{}, fmt.Errorf("%w: Port must be between 0 and 65535", ErrInvalidConfig)
	}
	if config.ProbePath == "" {
		config.ProbePath = DefaultProbePath
	}
	if config.NotifyPath == "" {
		config.NotifyPath = DefaultNotifyPath
	}
	if err := validateBuiltInPath("ProbePath", config.ProbePath); err != nil {
		return Config{}, err
	}
	if err := validateBuiltInPath("NotifyPath", config.NotifyPath); err != nil {
		return Config{}, err
	}
	if config.ProbePath == config.NotifyPath {
		return Config{}, fmt.Errorf("%w: ProbePath and NotifyPath must differ", ErrInvalidConfig)
	}
	if config.ProbeTimeoutMs < 0 || config.ProbeTimeoutMs > math.MaxInt32 {
		return Config{}, fmt.Errorf("%w: ProbeTimeoutMs must be between 0 and %d", ErrInvalidConfig, math.MaxInt32)
	}
	if config.ProbeTimeoutMs == 0 {
		config.ProbeTimeoutMs = DefaultProbeTimeoutMs
	}
	if config.RegisterRetry < 0 {
		return Config{}, fmt.Errorf("%w: RegisterRetry must not be negative", ErrInvalidConfig)
	}
	if config.RegisterRetry == 0 {
		config.RegisterRetry = DefaultRegisterRetry
	}
	if config.Serve == nil {
		config.Serve = serveNetHTTP
	}
	return config, nil
}

func validateBuiltInPath(field, path string) error {
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

func normalizeRoutes(routes []Route) ([]Route, error) {
	normalized := make([]Route, len(routes))
	seenRoutes := make(map[string]struct{}, len(routes))
	seenOperationIDs := make(map[string]struct{})
	for index, route := range routes {
		method := strings.ToUpper(strings.TrimSpace(route.Method))
		if !validMethod(method) {
			return nil, fmt.Errorf("%w: route %d has unsupported method %q", ErrInvalidConfig, index, route.Method)
		}
		if !strings.HasPrefix(route.Path, "/") {
			return nil, fmt.Errorf("%w: route %d path %q must start with '/'", ErrInvalidConfig, index, route.Path)
		}
		if strings.ContainsAny(route.Path, " \t\r\n?#") {
			return nil, fmt.Errorf("%w: route %d path %q contains invalid characters", ErrInvalidConfig, index, route.Path)
		}
		key := method + " " + route.Path
		if _, exists := seenRoutes[key]; exists {
			return nil, fmt.Errorf("%w: duplicate route %s", ErrInvalidConfig, key)
		}
		seenRoutes[key] = struct{}{}
		if route.OperationID != "" {
			if _, exists := seenOperationIDs[route.OperationID]; exists {
				return nil, fmt.Errorf("%w: duplicate operationId %q", ErrInvalidConfig, route.OperationID)
			}
			seenOperationIDs[route.OperationID] = struct{}{}
		}
		for status, response := range route.Responses {
			if strings.TrimSpace(status) == "" {
				return nil, fmt.Errorf("%w: route %s has an empty response status", ErrInvalidConfig, key)
			}
			switch response.(type) {
			case string, map[string]any:
			default:
				return nil, fmt.Errorf("%w: route %s response %q must be a description string or Response Object", ErrInvalidConfig, key, status)
			}
		}
		route.Method = method
		normalized[index] = route
	}
	return normalized, nil
}

func validMethod(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut,
		http.MethodPatch, http.MethodDelete, http.MethodOptions, http.MethodTrace, http.MethodConnect:
		return true
	default:
		return false
	}
}

func (p *Provider) buildHandler() (http.Handler, error) {
	if err := checkBusinessConflicts(p.cfg.Handler, p.cfg.ProbePath, p.cfg.NotifyPath); err != nil {
		return nil, err
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+p.cfg.ProbePath, p.handleHealth)
	mux.HandleFunc("POST "+p.cfg.NotifyPath, p.handleNotify)
	if p.cfg.Handler != nil {
		mux.Handle("/", p.cfg.Handler)
	}
	return mux, nil
}

func (p *Provider) handleHealth(w http.ResponseWriter, r *http.Request) {
	if p.cfg.HealthHandler != nil {
		p.cfg.HealthHandler(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{
		"status":  "ok",
		"service": p.cfg.Name,
		"version": p.cfg.Version,
	})
}

type notifyEvent struct {
	Type        string `json:"type"`
	NextBaseURL string `json:"nextBaseUrl"`
	NextSnake   string `json:"next_base_url"`
}

func (p *Provider) handleNotify(w http.ResponseWriter, r *http.Request) {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	var event notifyEvent
	if err := decoder.Decode(&event); err != nil {
		http.Error(w, `{"code":"invalid_request","message":"malformed notification body"}`, http.StatusBadRequest)
		return
	}
	if event.Type != "gateway_restarting" {
		// 其它事件由调用方按需处理；库只消费重启预通知。
		w.WriteHeader(http.StatusNoContent)
		return
	}

	nextBaseURL := event.NextBaseURL
	if nextBaseURL == "" {
		nextBaseURL = event.NextSnake
	}
	if nextBaseURL == "" {
		http.Error(w, `{"code":"invalid_request","message":"gateway_restarting requires nextBaseUrl"}`, http.StatusBadRequest)
		return
	}

	client := p.gatewayClient()
	if client == nil {
		http.Error(w, `{"code":"unavailable","message":"provider is not started"}`, http.StatusServiceUnavailable)
		return
	}
	if err := client.SetBaseURL(nextBaseURL); err != nil {
		http.Error(w, `{"code":"invalid_request","message":"nextBaseUrl is not a valid http(s) URL"}`, http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (p *Provider) gatewayClient() *localgateway.Client {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.client
}

// checkBusinessConflicts 用合成请求探测业务 handler 是否已占用内建路径。
// 返回 404 视为未处理；任何其它响应或 panic 都视为冲突，避免静默覆盖。
func checkBusinessConflicts(handler http.Handler, probePath, notifyPath string) error {
	if handler == nil {
		return nil
	}
	checks := []struct {
		method string
		path   string
	}{
		{http.MethodGet, probePath},
		{http.MethodPost, notifyPath},
	}
	for _, check := range checks {
		if businessHandlerServes(handler, check.method, check.path) {
			return fmt.Errorf("%w: business handler already serves %s %s", ErrRouteConflict, check.method, check.path)
		}
	}
	return nil
}

func businessHandlerServes(handler http.Handler, method, path string) (served bool) {
	request, err := http.NewRequest(method, "http://127.0.0.1"+path, nil)
	if err != nil {
		return true
	}
	recorder := &probeResponseWriter{}
	defer func() {
		if recover() != nil {
			served = true
		}
	}()
	handler.ServeHTTP(recorder, request)
	if !recorder.wrote {
		// net/http 在 handler 未写响应时隐式返回 200；能走到这里说明路径被匹配。
		return true
	}
	return recorder.status != http.StatusNotFound
}

type probeResponseWriter struct {
	header http.Header
	status int
	wrote  bool
}

func (w *probeResponseWriter) Header() http.Header {
	if w.header == nil {
		w.header = make(http.Header)
	}
	return w.header
}

func (w *probeResponseWriter) WriteHeader(status int) {
	if !w.wrote {
		w.status = status
		w.wrote = true
	}
}

func (w *probeResponseWriter) Write(body []byte) (int, error) {
	if !w.wrote {
		w.status = http.StatusOK
		w.wrote = true
	}
	return len(body), nil
}
