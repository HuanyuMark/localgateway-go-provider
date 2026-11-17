# LocalGateway Go Provider

在 [Huma](https://huma.rocks/) 之上补齐 LocalGateway provider 生命周期的 Go 库：
监听、内建探活/重启通知端点、注册与注销、注册重试、OpenAPI 上报和优雅退出。

模块路径为 `github.com/HuanyuMark/localgateway-go-provider`，当前版本 **v0.2.0**。
客户端与生成模型位于独立仓库 `github.com/HuanyuMark/localgateway-go`。

```text
localgateway-go-provider/
├─ builder.go     # New() 链式配置与 Build()
├─ builtins.go    # 内建 health / notify 操作
├─ openapi.go     # 从 huma.API 导出 OpenAPI
├─ provider.go    # 配置校验与 Provider 类型
├─ run.go         # 注册、重试、重启通知与退出生命周期
├─ serve.go       # ServeFunc 与 ServeNetHTTP
└─ go.mod         # module github.com/HuanyuMark/localgateway-go-provider
```

## 安装

```powershell
go get github.com/HuanyuMark/localgateway-go-provider@v0.2.0
```

## 快速开始

```go
package main

import (
	"context"
	"net/http"

	"github.com/HuanyuMark/localgateway-go-provider"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"
)

type helloOutput struct {
	Body struct {
		Message string `json:"message"`
	}
}

func main() {
	mux := http.NewServeMux()
	api := humago.New(mux, huma.DefaultConfig("my-service", "1.0.0"))

	huma.Register(api, huma.Operation{
		Method:      http.MethodGet,
		Path:        "/hello",
		OperationID: "getHello",
		Summary:     "Say hello",
	}, func(ctx context.Context, _ *struct{}) (*helloOutput, error) {
		out := &helloOutput{}
		out.Body.Message = "hello"
		return out, nil
	})

	p, err := provider.New().
		Name("my-service").
		Version("1.0.0").
		API(api).
		Serve(provider.ServeNetHTTP(mux)).
		Build()
	if err != nil {
		panic(err)
	}

	if err := p.Run(context.Background()); err != nil {
		panic(err)
	}
}
```

需要显式端口或退出时注销：

```go
p, _ := provider.New().
	Name("my-service").
	Version("1.0.0").
	Port(18080).
	UnregisterOnShutdown(true).
	API(api).
	Serve(provider.ServeNetHTTP(mux)).
	Build()
```

## Builder

| 方法 | 默认 | 说明 |
|---|---|---|
| `Name` / `Version` | 必填 | 服务名与版本 |
| `Port` | `0` | `0` 为临时端口；库只监听 `127.0.0.1` |
| `ProbePath` | `/health` | 内建健康检查端点 |
| `NotifyPath` | `/_localgateway/events` | 接收网关重启预通知的端点 |
| `ProbeTimeout` | `2000` | 随注册上报的探活超时（毫秒） |
| `ProjectRoot` | 空 | 非空时随注册上报 |
| `UnregisterOnShutdown` | `false` | 退出时 `DELETE /gw/services/{name}` |
| `RegisterRetry` | `30s` | 注册失败后的最长退避重试窗口 |
| `API` | 必填 | `huma.API`，业务路由与 OpenAPI 的来源 |
| `Serve` | 必填 | `ServeFunc`；net/http 用 `ServeNetHTTP(handler)` |
| `GatewayClient` | `nil` | 可选，复用已配置 PortReader 的 client |

`Build()` 不接收 `context.Context`；所有配置问题会累积到 `errors.Join` 中一并返回
（`errors.Is(err, provider.ErrInvalidConfig)`）。

生命周期：绑定 listener → 启动服务 → `PUT /gw/services/{name}`（网关在 PUT 内
同步探活；注册请求本身遇到可重试错误才按指数退避重试到 `RegisterRetry` 窗口结束）
→ 阻塞到 ctx 取消 → 可选 `DELETE` → 优雅停服。

## 内建端点

内建端点通过 `huma.Register` 注入：

- `GET <ProbePath>` 默认返回 `{"status":"ok","service":...,"version":...}`。
- `POST <NotifyPath>` 解析 `gateway_restarting` 的 `nextBaseUrl`（兼容
  `next_base_url`），刷新管理面基地址，后续注册/PATCH 使用新地址；非法通知返回 4xx。

若用户在 `Build()` 之前已经注册了同路径同方法的操作，provider 保留用户定义并跳过
内建端点；重复使用 `getHealth` / `gatewayEvent` 作为 operationId 会让 `Build()`
返回 `ErrInvalidConfig`。

### 自定义健康检查

需要按业务状态返回 503 或自定义 JSON 时，直接注册自己的 `GET <ProbePath>` 操作：

```go
huma.Register(api, huma.Operation{
	Method:      http.MethodGet,
	Path:        "/health",
	OperationID: "customHealth",
}, func(ctx context.Context, _ *struct{}) (*healthOutput, error) {
	return checkHealth(ctx)
})
```

网关探活只把 2xx 视为存活；非 2xx 记为一次 miss，服务状态会降为 `degraded`，
连续 miss 达到 `offlineAfterMisses` 后转为 `offline`。注册时首次探活失败不会拒绝
注册，只是初始状态为 `degraded`。

## 自定义框架适配器

`ServeFunc` 的契约：在 listener 上阻塞服务，并在 ctx 取消后停止、返回。

```go
type ServeFunc func(ctx context.Context, listener net.Listener) error

func ServeNetHTTP(handler http.Handler) ServeFunc
```

`*gin.Engine`、`*echo.Echo` 等都实现 `http.Handler`，直接传给 `ServeNetHTTP`；
框架自带 listener 时实现自己的 `ServeFunc`：

```go
p, _ := provider.New().
	Name("gin-service").
	Version("1.0.0").
	API(api).
	Serve(func(ctx context.Context, listener net.Listener) error {
		return app.Listener(listener) // 例如 fiber
	}).
	Build()
```

## OpenAPI

OpenAPI 文档由 Huma 维护，`Build()` 从 `api.OpenAPI()` 导出并在注册时作为
`apiInfo` 一次上报。自定义文档直接设置 `huma.Config.OpenAPI`：

```go
cfg := huma.DefaultConfig("my-service", "1.0.0")
cfg.OpenAPI = &huma.OpenAPI{
	OpenAPI: "3.0.3",
	Info:    &huma.Info{Title: "custom", Version: "9.9.9"},
}
api := humago.New(mux, cfg)
```

Provider 不做版本校验，也不额外合并文档。

## 通过网关模块代理引入

```powershell
$env:GOPROXY = "http://127.0.0.1:47821/gw/goproxy,https://goproxy.cn,direct"
$env:GONOSUMDB = "github.com/HuanyuMark/*"
go get github.com/HuanyuMark/localgateway-go-provider@v0.2.0
go mod tidy
```

逗号列表让网关返回 404 时回退到公共代理；huma 等第三方依赖仍由公共代理提供。

## 测试

```powershell
gofmt -l .      # 应无输出
go vet ./...
go test ./...
go build ./...
```

- `builder_test.go`：Builder 链式/默认值/错误累积、内建端点自动注册与用户覆盖、
  重复 operationId。
- `openapi_test.go`：`fromHuma` 导出自动生成与用户自定义的 OpenAPI。
- `provider_test.go`：默认健康检查 JSON、用户覆盖、通知刷新
  `nextBaseUrl`/`nextBase_url`、非法通知、405、自定义 `ServeFunc`。
- `run_test.go`：最小配置注册、自定义内建路径、注册退避重试、退出注销。

## 限制

- 服务只监听 `127.0.0.1`；对外暴露由 LocalGateway 负责。
- 契约首版只支持 `http` 协议；`Protocol` 固定发送 `http`。
- 重启预通知只刷新基地址，不自动重新注册。
- 端口发现依赖 Windows 注册表；其它平台请通过 `GatewayClient` 注入已配置
  `PortReader` 的客户端。
