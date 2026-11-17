package provider

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	localgateway "github.com/HuanyuMark/localgateway-go/client"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"
)

func newTestHumaAPI(t *testing.T) (huma.API, *http.ServeMux) {
	t.Helper()
	mux := http.NewServeMux()
	return humago.New(mux, huma.DefaultConfig("demo", "1.0.0")), mux
}

func validBuilder(t *testing.T) *Builder {
	t.Helper()
	api, mux := newTestHumaAPI(t)
	return New().
		Name("demo").
		Version("1.0.0").
		API(api).
		Serve(ServeNetHTTP(mux))
}

func TestBuilderChain(t *testing.T) {
	api, mux := newTestHumaAPI(t)
	client, err := localgateway.New(localgateway.Options{BaseURL: "http://127.0.0.1:1"})
	if err != nil {
		t.Fatalf("localgateway.New() error = %v", err)
	}

	builder := New().
		Name("svc").
		Version("2.0.0").
		Port(18080).
		ProbePath("/readyz").
		NotifyPath("/events").
		ProbeTimeout(1500).
		ProjectRoot(`D:\root`).
		UnregisterOnShutdown(true).
		RegisterRetry(5 * time.Second).
		API(api).
		Serve(ServeNetHTTP(mux)).
		GatewayClient(client)

	provider, err := builder.Build()
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	if provider.cfg.Name != "svc" || provider.cfg.Version != "2.0.0" {
		t.Errorf("cfg name/version = %q/%q", provider.cfg.Name, provider.cfg.Version)
	}
	if provider.cfg.Port != 18080 {
		t.Errorf("cfg.Port = %d, want 18080", provider.cfg.Port)
	}
	if provider.cfg.ProbePath != "/readyz" || provider.cfg.NotifyPath != "/events" {
		t.Errorf("cfg paths = %q/%q", provider.cfg.ProbePath, provider.cfg.NotifyPath)
	}
	if provider.cfg.ProbeTimeoutMs != 1500 {
		t.Errorf("cfg.ProbeTimeoutMs = %d, want 1500", provider.cfg.ProbeTimeoutMs)
	}
	if provider.cfg.ProjectRoot != `D:\root` {
		t.Errorf("cfg.ProjectRoot = %q", provider.cfg.ProjectRoot)
	}
	if !provider.cfg.UnregisterOnShutdown {
		t.Error("cfg.UnregisterOnShutdown = false, want true")
	}
	if provider.cfg.RegisterRetry != 5*time.Second {
		t.Errorf("cfg.RegisterRetry = %v, want 5s", provider.cfg.RegisterRetry)
	}
	if provider.cfg.GatewayClient != client {
		t.Error("cfg.GatewayClient was not preserved")
	}
	if provider.serve == nil {
		t.Error("serve func was not preserved")
	}
}

func TestBuilderDefaults(t *testing.T) {
	api, mux := newTestHumaAPI(t)
	provider, err := New().
		Name("demo").
		Version("1.0.0").
		API(api).
		Serve(ServeNetHTTP(mux)).
		Build()
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	if provider.cfg.ProbePath != DefaultProbePath {
		t.Errorf("ProbePath = %q, want %q", provider.cfg.ProbePath, DefaultProbePath)
	}
	if provider.cfg.NotifyPath != DefaultNotifyPath {
		t.Errorf("NotifyPath = %q, want %q", provider.cfg.NotifyPath, DefaultNotifyPath)
	}
	if provider.cfg.ProbeTimeoutMs != DefaultProbeTimeoutMs {
		t.Errorf("ProbeTimeoutMs = %d, want %d", provider.cfg.ProbeTimeoutMs, DefaultProbeTimeoutMs)
	}
	if provider.cfg.RegisterRetry != DefaultRegisterRetry {
		t.Errorf("RegisterRetry = %v, want %v", provider.cfg.RegisterRetry, DefaultRegisterRetry)
	}
}

func TestBuilderValidation(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Builder)
	}{
		{name: "empty name", mutate: func(b *Builder) { b.name = " " }},
		{name: "empty version", mutate: func(b *Builder) { b.version = "" }},
		{name: "missing API", mutate: func(b *Builder) { b.api = nil }},
		{name: "missing Serve", mutate: func(b *Builder) { b.serve = nil }},
		{name: "negative port", mutate: func(b *Builder) { b.port = -1 }},
		{name: "port too large", mutate: func(b *Builder) { b.port = 65536 }},
		{name: "relative probe path", mutate: func(b *Builder) { b.probePath = "health" }},
		{name: "root probe path", mutate: func(b *Builder) { b.probePath = "/" }},
		{name: "relative notify path", mutate: func(b *Builder) { b.notifyPath = "events" }},
		{name: "trailing slash notify path", mutate: func(b *Builder) { b.notifyPath = "/events/" }},
		{name: "same built-in paths", mutate: func(b *Builder) { b.probePath = "/probe"; b.notifyPath = "/probe" }},
		{name: "negative probe timeout", mutate: func(b *Builder) { b.probeTimeoutMs = -1 }},
		{name: "negative register retry", mutate: func(b *Builder) { b.registerRetry = -time.Second }},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			builder := validBuilder(t)
			testCase.mutate(builder)
			if _, err := builder.Build(); !errors.Is(err, ErrInvalidConfig) {
				t.Fatalf("Build() error = %v, want ErrInvalidConfig", err)
			}
		})
	}
}

func TestBuilderCumulatesErrors(t *testing.T) {
	_, err := New().Build()
	if !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("Build() error = %v, want ErrInvalidConfig", err)
	}
	message := err.Error()
	for _, fragment := range []string{
		"Name is required",
		"Version is required",
		"API is required",
		"Serve is required",
	} {
		if !strings.Contains(message, fragment) {
			t.Errorf("Build() error = %q, missing %q", message, fragment)
		}
	}
}

func TestBuilderRegistersBuiltins(t *testing.T) {
	api, mux := newTestHumaAPI(t)
	provider, err := New().
		Name("demo").
		Version("1.0.0").
		API(api).
		Serve(ServeNetHTTP(mux)).
		Build()
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}

	paths, ok := provider.spec["paths"].(map[string]any)
	if !ok {
		t.Fatalf("spec paths = %v", provider.spec["paths"])
	}
	healthPath, ok := paths[DefaultProbePath].(map[string]any)
	if !ok {
		t.Fatalf("spec path %s = %v", DefaultProbePath, paths[DefaultProbePath])
	}
	healthGet, ok := healthPath["get"].(map[string]any)
	if !ok || healthGet["operationId"] != healthOperationID {
		t.Errorf("health get = %v, want operationId %q", healthPath["get"], healthOperationID)
	}

	notifyPath, ok := paths[DefaultNotifyPath].(map[string]any)
	if !ok {
		t.Fatalf("spec path %s = %v", DefaultNotifyPath, paths[DefaultNotifyPath])
	}
	notifyPost, ok := notifyPath["post"].(map[string]any)
	if !ok || notifyPost["operationId"] != notifyOperationID {
		t.Errorf("notify post = %v, want operationId %q", notifyPath["post"], notifyOperationID)
	}
}

func TestBuilderNotifySchemaAvoidsDuplicateCodegenName(t *testing.T) {
	api, mux := newTestHumaAPI(t)
	provider, err := New().
		Name("demo").
		Version("1.0.0").
		API(api).
		Serve(ServeNetHTTP(mux)).
		Build()
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}

	components, ok := provider.spec["components"].(map[string]any)
	if !ok {
		t.Fatalf("spec components = %v", provider.spec["components"])
	}
	schemas, ok := components["schemas"].(map[string]any)
	if !ok {
		t.Fatalf("spec schemas = %v", components["schemas"])
	}
	notify, ok := schemas["NotifyPayload"].(map[string]any)
	if !ok {
		t.Fatalf("NotifyPayload schema = %v", schemas["NotifyPayload"])
	}
	properties, ok := notify["properties"].(map[string]any)
	if !ok {
		t.Fatalf("NotifyPayload properties = %v", notify["properties"])
	}
	if _, ok := properties["nextBaseUrl"]; !ok {
		t.Errorf("NotifyPayload lacks nextBaseUrl: %v", properties)
	}
	// 通知端点固定使用 camelCase 的 nextBaseUrl；next_base_url 只出现在 SSE
	// 事件 data 中，是另一种载荷。schema 不得同时登记两个属性：openapi-generator
	// 的 Go 生成器会把它们都归一成 NextBaseUrl，导致生成客户端重复声明字段。
	if _, ok := properties["next_base_url"]; ok {
		t.Errorf("NotifyPayload documents next_base_url alongside nextBaseUrl: %v", properties)
	}
}

func TestBuilderCustomBuiltinPaths(t *testing.T) {
	api, mux := newTestHumaAPI(t)
	provider, err := New().
		Name("demo").
		Version("1.0.0").
		ProbePath("/readyz").
		NotifyPath("/events").
		API(api).
		Serve(ServeNetHTTP(mux)).
		Build()
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}

	paths := provider.spec["paths"].(map[string]any)
	if _, ok := paths["/readyz"]; !ok {
		t.Error("spec lacks /readyz")
	}
	if _, ok := paths["/events"]; !ok {
		t.Error("spec lacks /events")
	}
	if _, ok := paths[DefaultProbePath]; ok {
		t.Error("spec unexpectedly contains the default /health path")
	}
	if _, ok := paths[DefaultNotifyPath]; ok {
		t.Error("spec unexpectedly contains the default notify path")
	}
}

func TestBuilderDuplicateOperationIDRecovered(t *testing.T) {
	api, mux := newTestHumaAPI(t)
	huma.Register(api, huma.Operation{
		Method:      http.MethodGet,
		Path:        "/custom",
		OperationID: healthOperationID,
	}, func(_ context.Context, _ *struct{}) (*struct{}, error) {
		return &struct{}{}, nil
	})

	_, err := New().
		Name("demo").
		Version("1.0.0").
		API(api).
		Serve(ServeNetHTTP(mux)).
		Build()
	if !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("Build() error = %v, want ErrInvalidConfig", err)
	}
	if !strings.Contains(err.Error(), "duplicate operation ID") {
		t.Errorf("Build() error = %q, want duplicate operation ID detail", err)
	}
}

func TestBuildDoesNotAcceptContext(t *testing.T) {
	// 编译期约束：Build 无参数，保持纯粹。
	var build func() (*Provider, error) = New().Name("demo").Version("1.0.0").Build
	_ = build
}
