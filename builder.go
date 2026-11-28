package provider

import (
	"errors"
	"fmt"
	"time"

	localgateway "github.com/HuanyuMark/localgateway-go/client"

	"github.com/danielgtaylor/huma/v2"
)

// Builder 以链式 API 创建 Provider。
//
// 必需项是 Name、Version、API 与 Serve；其余字段都有默认值（跨域默认一键
// 放行，可用 NoCORS 关闭）。链式方法只记录原始值，Build 统一归一化与校验，
// 并把所有问题用 errors.Join 一起返回。
type Builder struct {
	name                 string
	version              string
	port                 int
	probePath            string
	notifyPath           string
	probeTimeoutMs       int
	projectRoot          string
	unregisterOnShutdown bool
	registerRetry        time.Duration
	cors                 *localgateway.Cors
	noCORS               bool
	api                  huma.API
	serve                ServeFunc
	gatewayClient        *localgateway.Client
}

// New 创建一个空的 Builder。
func New() *Builder {
	return &Builder{}
}

// Name 设置注册的服务名（必填）。
func (b *Builder) Name(name string) *Builder {
	b.name = name
	return b
}

// Version 设置 provider 版本（必填）。
func (b *Builder) Version(version string) *Builder {
	b.version = version
	return b
}

// Port 设置监听端口；0 表示由系统分配临时端口。
func (b *Builder) Port(port int) *Builder {
	b.port = port
	return b
}

// ProbePath 设置内建健康检查路径，默认 /health。
func (b *Builder) ProbePath(path string) *Builder {
	b.probePath = path
	return b
}

// NotifyPath 设置接收网关重启预通知的路径，默认 /_localgateway/events。
func (b *Builder) NotifyPath(path string) *Builder {
	b.notifyPath = path
	return b
}

// ProbeTimeout 设置随注册上报的探活超时（毫秒），默认 2000。
func (b *Builder) ProbeTimeout(ms int) *Builder {
	b.probeTimeoutMs = ms
	return b
}

// ProjectRoot 设置可选的工程根目录；非空时随注册信息上报。
func (b *Builder) ProjectRoot(root string) *Builder {
	b.projectRoot = root
	return b
}

// UnregisterOnShutdown 设置退出时是否 DELETE 注册。
func (b *Builder) UnregisterOnShutdown(unregister bool) *Builder {
	b.unregisterOnShutdown = unregister
	return b
}

// CORS 设置服务级跨域配置；注册时一并上报，由网关统一应答预检与改写
// Access-Control-* 响应头。未调用任何跨域方法时默认为
// Cors{AllowAll: true}，即一键放行所有域。
func (b *Builder) CORS(cors localgateway.Cors) *Builder {
	b.cors = &cors
	b.noCORS = false
	return b
}

// AllowAllCORS 一键放行所有来源、方法与请求头；这也是默认行为。
func (b *Builder) AllowAllCORS() *Builder {
	return b.CORS(localgateway.Cors{AllowAll: true})
}

// NoCORS 关闭默认跨域：注册时不携带 cors，网关完全透传 provider 自己的
// Access-Control-* 响应头。
func (b *Builder) NoCORS() *Builder {
	b.cors = nil
	b.noCORS = true
	return b
}

// RegisterRetry 设置注册失败后的最长重试窗口，默认 30s。
func (b *Builder) RegisterRetry(window time.Duration) *Builder {
	b.registerRetry = window
	return b
}

// API 设置业务路由与 OpenAPI 所在的 Huma API（必填）。
func (b *Builder) API(api huma.API) *Builder {
	b.api = api
	return b
}

// Serve 设置服务启动函数（必填）。net/http 场景用 ServeNetHTTP。
func (b *Builder) Serve(serve ServeFunc) *Builder {
	b.serve = serve
	return b
}

// GatewayClient 可选：复用已创建的管理面客户端（测试或自定义 PortReader）。
// 为空时在启动阶段用默认 Options 创建。
func (b *Builder) GatewayClient(client *localgateway.Client) *Builder {
	b.gatewayClient = client
	return b
}

// Build 校验配置、注册内建端点并导出 OpenAPI 文档，但不监听端口。
//
// 配置问题会累积到 errors.Join 中一并返回。Build 不接收 context。
func (b *Builder) Build() (*Provider, error) {
	if b == nil {
		return nil, fmt.Errorf("%w: builder is nil", ErrInvalidConfig)
	}

	cfg, configErr := normalizeProviderConfig(providerConfig{
		Name:                 b.name,
		Version:              b.version,
		Port:                 b.port,
		ProbePath:            b.probePath,
		ProbeTimeoutMs:       b.probeTimeoutMs,
		ProjectRoot:          b.projectRoot,
		NotifyPath:           b.notifyPath,
		UnregisterOnShutdown: b.unregisterOnShutdown,
		RegisterRetry:        b.registerRetry,
		CORS:                 b.cors,
		NoCORS:               b.noCORS,
		GatewayClient:        b.gatewayClient,
	})

	var errs []error
	if configErr != nil {
		errs = append(errs, configErr)
	}
	if b.api == nil {
		errs = append(errs, fmt.Errorf("%w: API is required", ErrInvalidConfig))
	}
	if b.serve == nil {
		errs = append(errs, fmt.Errorf("%w: Serve is required", ErrInvalidConfig))
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}

	p := &Provider{cfg: cfg, serve: b.serve}
	if err := p.registerBuiltins(b.api); err != nil {
		return nil, err
	}
	spec, err := fromHuma(b.api)
	if err != nil {
		return nil, err
	}
	p.spec = spec
	return p, nil
}
