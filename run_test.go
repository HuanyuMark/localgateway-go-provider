package provider

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	localgateway "github.com/HuanyuMark/localgateway-go/client"

	"github.com/danielgtaylor/huma/v2"
)

type helloOutput struct {
	Body struct {
		Message string `json:"message"`
	}
}

func registerHello(api huma.API) {
	huma.Register(api, huma.Operation{
		Method:      http.MethodGet,
		Path:        "/hello",
		OperationID: "hello",
		Summary:     "Say hello",
	}, func(_ context.Context, _ *struct{}) (*helloOutput, error) {
		output := &helloOutput{}
		output.Body.Message = "hello"
		return output, nil
	})
}

func TestRunRegistersMinimalConfig(t *testing.T) {
	gateway := newMockGateway(t)
	provider, _, _ := newHumaTestProvider(t, gateway, "demo", "1.2.3", func(api huma.API, _ *http.ServeMux) {
		registerHello(api)
	}, nil)
	startProvider(t, provider)

	waitFor(t, 5*time.Second, "registration", func() bool {
		puts, _, _ := gateway.counts()
		return puts >= 1 && providerRegistered(provider)
	})
	if provider.Port() == 0 {
		t.Fatal("Port() = 0, want the actual listener port")
	}

	request, ok := gateway.lastRequest(http.MethodPut)
	if !ok {
		t.Fatal("no PUT request recorded")
	}
	if request.path != "/gw/services/demo" {
		t.Errorf("PUT path = %q", request.path)
	}
	if request.body["port"] != float64(provider.Port()) {
		t.Errorf("port = %v, want %d", request.body["port"], provider.Port())
	}
	if request.body["version"] != "1.2.3" {
		t.Errorf("version = %v", request.body["version"])
	}
	if request.body["protocol"] != "http" {
		t.Errorf("protocol = %v", request.body["protocol"])
	}
	probe, ok := request.body["probe"].(map[string]any)
	if !ok || probe["path"] != "/health" || probe["timeoutMs"] != float64(2000) {
		t.Errorf("probe = %v", request.body["probe"])
	}
	wantNotify := fmt.Sprintf("http://127.0.0.1:%d/_localgateway/events", provider.Port())
	if request.body["notifyUrl"] != wantNotify {
		t.Errorf("notifyUrl = %v, want %s", request.body["notifyUrl"], wantNotify)
	}
	apiInfo, ok := request.body["apiInfo"].(map[string]any)
	if !ok || apiInfo["openapi"] != "3.1.0" {
		t.Fatalf("apiInfo = %v, want exported Huma OpenAPI", request.body["apiInfo"])
	}
	paths, ok := apiInfo["paths"].(map[string]any)
	if !ok {
		t.Fatalf("apiInfo.paths = %v", apiInfo["paths"])
	}
	if _, ok := paths["/health"]; !ok {
		t.Error("OpenAPI lacks /health")
	}
	if _, ok := paths["/hello"]; !ok {
		t.Error("OpenAPI lacks /hello")
	}

	if status, _ := httpGet(t, fmt.Sprintf("http://127.0.0.1:%d/health", provider.Port())); status != http.StatusOK {
		t.Errorf("GET /health status = %d, want 200", status)
	}
	status, body := httpGet(t, fmt.Sprintf("http://127.0.0.1:%d/hello", provider.Port()))
	if status != http.StatusOK || !strings.Contains(string(body), "hello") {
		t.Errorf("GET /hello = %d %q, want 200 hello", status, body)
	}

	if _, _, deletes := gateway.counts(); deletes != 0 {
		t.Errorf("DELETE calls = %d, want 0", deletes)
	}
}

func TestCustomProbeAndNotifyPaths(t *testing.T) {
	gateway := newMockGateway(t)
	provider, _, _ := newHumaTestProvider(t, gateway, "custom-paths", "0.1.0", nil, func(builder *Builder) {
		builder.ProbePath("/readyz").NotifyPath("/events/localgateway")
	})
	startProvider(t, provider)
	waitFor(t, 5*time.Second, "registration", func() bool {
		puts, _, _ := gateway.counts()
		return puts >= 1 && providerRegistered(provider)
	})

	request, _ := gateway.lastRequest(http.MethodPut)
	probe := request.body["probe"].(map[string]any)
	if probe["path"] != "/readyz" {
		t.Errorf("probe.path = %v", probe["path"])
	}
	wantNotify := fmt.Sprintf("http://127.0.0.1:%d/events/localgateway", provider.Port())
	if request.body["notifyUrl"] != wantNotify {
		t.Errorf("notifyUrl = %v, want %s", request.body["notifyUrl"], wantNotify)
	}
	if status, _ := httpGet(t, fmt.Sprintf("http://127.0.0.1:%d/readyz", provider.Port())); status != http.StatusOK {
		t.Errorf("GET /readyz status = %d", status)
	}
	if status, _ := httpGet(t, fmt.Sprintf("http://127.0.0.1:%d/health", provider.Port())); status != http.StatusNotFound {
		t.Errorf("GET /health status = %d, want 404 for custom probe path", status)
	}
}

func TestUnregisterOnShutdown(t *testing.T) {
	gateway := newMockGateway(t)
	provider, _, _ := newHumaTestProvider(t, gateway, "demo", "1.0.0", nil, func(builder *Builder) {
		builder.UnregisterOnShutdown(true)
	})
	stop := startProvider(t, provider)
	waitFor(t, 5*time.Second, "registration", func() bool {
		puts, _, _ := gateway.counts()
		return puts >= 1 && providerRegistered(provider)
	})
	if err := stop(); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if _, _, deletes := gateway.counts(); deletes != 1 {
		t.Errorf("DELETE calls = %d, want 1", deletes)
	}
}

func TestRegisterRetriesUntilGatewayAccepts(t *testing.T) {
	gateway := newMockGateway(t)
	gateway.putFailures = 2
	provider, _, _ := newHumaTestProvider(t, gateway, "demo", "1.0.0", nil, func(builder *Builder) {
		builder.RegisterRetry(5 * time.Second)
	})

	stop := startProvider(t, provider)
	waitFor(t, 5*time.Second, "successful registration", func() bool {
		puts, _, _ := gateway.counts()
		return puts >= 3 && providerRegistered(provider)
	})
	if provider.Port() == 0 {
		t.Fatal("Port() = 0 after registration")
	}
	if err := stop(); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if puts, _, _ := gateway.counts(); puts != 3 {
		t.Errorf("PUT calls = %d, want 3", puts)
	}
}

func TestInvalidCORSFailsFastWithoutRetry(t *testing.T) {
	gateway := newMockGateway(t)
	provider, _, _ := newHumaTestProvider(t, gateway, "demo", "1.0.0", nil, func(builder *Builder) {
		// 既不放行所有域又没有来源，客户端会在本地校验失败，属于配置错误。
		builder.CORS(localgateway.Cors{})
		builder.RegisterRetry(5 * time.Second)
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	start := time.Now()
	err := provider.Start(ctx)
	if err == nil {
		t.Fatal("Start() error = nil, want ErrInvalidCORS")
	}
	if !errors.Is(err, localgateway.ErrInvalidCORS) {
		t.Fatalf("Start() error = %v, want ErrInvalidCORS", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("Start() returned after %v, want an immediate failure without backoff", elapsed)
	}
	if puts, _, _ := gateway.counts(); puts != 0 {
		t.Errorf("PUT calls = %d, want 0 for a locally rejected config", puts)
	}
}

func TestRegistrationStopsAfterRetryWindow(t *testing.T) {
	gateway := newMockGateway(t)
	gateway.putFailures = 100
	provider, _, _ := newHumaTestProvider(t, gateway, "demo", "1.0.0", nil, func(builder *Builder) {
		builder.RegisterRetry(250 * time.Millisecond)
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	start := time.Now()
	err := provider.Start(ctx)
	if err == nil {
		t.Fatal("Start() error = nil, want retry window error")
	}
	if elapsed := time.Since(start); elapsed < 200*time.Millisecond {
		t.Errorf("Start() returned after %v, want at least one backoff", elapsed)
	}
	puts, _, _ := gateway.counts()
	if puts < 2 {
		t.Errorf("PUT calls = %d, want at least 2", puts)
	}
}
