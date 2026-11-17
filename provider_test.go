package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	localgateway "github.com/HuanyuMark/localgateway-go/client"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"
)

type recordedRequest struct {
	method string
	path   string
	body   map[string]any
}

type mockGateway struct {
	mu           sync.Mutex
	server       *httptest.Server
	requests     []recordedRequest
	sessionCalls int
	putCalls     int
	patchCalls   int
	deleteCalls  int
	putFailures  int
}

func newMockGateway(t *testing.T) *mockGateway {
	t.Helper()
	gateway := &mockGateway{}
	gateway.server = httptest.NewServer(http.HandlerFunc(gateway.handle))
	t.Cleanup(gateway.server.Close)
	return gateway
}

func (g *mockGateway) handle(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	body := map[string]any{}
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &body)
	}

	g.mu.Lock()
	g.requests = append(g.requests, recordedRequest{method: r.Method, path: r.URL.Path, body: body})
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/gw/session":
		g.sessionCalls++
		g.mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{
			"token":     "tok-test",
			"expiresAt": "2026-11-14T12:00:00Z",
			"gateway":   map[string]any{"baseUrl": "http://127.0.0.1:47821", "version": "0.1.0", "pid": 100},
			"caller":    map[string]any{"pid": 200, "userSid": "S-1-5-21-1", "imagePath": `C:\provider.exe`},
			"policy":    map[string]any{"probeIntervalMs": 10000, "probeTimeoutMs": 2000, "offlineAfterMisses": 3, "expireAfterMs": 60000},
		})
	case r.Method == http.MethodPut && strings.HasPrefix(r.URL.Path, "/gw/services/"):
		g.putCalls++
		fail := g.putCalls <= g.putFailures
		g.mu.Unlock()
		if fail {
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{
				"code": "unavailable", "message": "starting",
			})
			return
		}
		writeJSON(w, http.StatusCreated, registrationJSON(body))
	case r.Method == http.MethodPatch && strings.HasPrefix(r.URL.Path, "/gw/services/"):
		g.patchCalls++
		g.mu.Unlock()
		writeJSON(w, http.StatusOK, registrationJSON(body))
	case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/gw/services/"):
		g.deleteCalls++
		g.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	default:
		g.mu.Unlock()
		http.NotFound(w, r)
	}
}

func registrationJSON(request map[string]any) map[string]any {
	name, _ := request["name"].(string)
	if name == "" {
		name = "demo"
	}
	return map[string]any{"name": name, "status": "healthy", "revision": 1}
}

func (g *mockGateway) counts() (puts, patches, deletes int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.putCalls, g.patchCalls, g.deleteCalls
}

func (g *mockGateway) lastRequest(method string) (recordedRequest, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for index := len(g.requests) - 1; index >= 0; index-- {
		if g.requests[index].method == method {
			return g.requests[index], true
		}
	}
	return recordedRequest{}, false
}

func gatewayClient(t *testing.T, baseURL string) *localgateway.Client {
	t.Helper()
	client, err := localgateway.New(localgateway.Options{BaseURL: baseURL})
	if err != nil {
		t.Fatalf("localgateway.New() error = %v", err)
	}
	return client
}

// newHumaTestProvider 创建 humago API 与 Provider；prepare 在 Build 之前注册业务操作。
func newHumaTestProvider(
	t *testing.T,
	gateway *mockGateway,
	name string,
	version string,
	prepare func(api huma.API, mux *http.ServeMux),
	mutate func(*Builder),
) (*Provider, huma.API, *http.ServeMux) {
	t.Helper()
	mux := http.NewServeMux()
	api := humago.New(mux, huma.DefaultConfig(name, version))
	if prepare != nil {
		prepare(api, mux)
	}

	builder := New().
		Name(name).
		Version(version).
		API(api).
		Serve(ServeNetHTTP(mux))
	if gateway != nil {
		builder.GatewayClient(gatewayClient(t, gateway.server.URL))
	}
	if mutate != nil {
		mutate(builder)
	}

	provider, err := builder.Build()
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	return provider, api, mux
}

// startProvider 在后台运行 Provider，并在测试结束时取消与等待 Run 返回。
func startProvider(t *testing.T, provider *Provider) func() error {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- provider.Run(ctx) }()

	var once sync.Once
	var runErr error
	stop := func() error {
		once.Do(func() {
			cancel()
			select {
			case runErr = <-done:
			case <-time.After(10 * time.Second):
				runErr = errors.New("provider did not stop")
			}
		})
		return runErr
	}
	t.Cleanup(func() {
		if err := stop(); err != nil {
			t.Errorf("Run() error = %v", err)
		}
	})
	return stop
}

func providerRegistered(provider *Provider) bool {
	provider.mu.Lock()
	defer provider.mu.Unlock()
	return provider.registered
}

func waitFor(t *testing.T, timeout time.Duration, description string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", description)
}

func httpGet(t *testing.T, url string) (int, []byte) {
	t.Helper()
	client := &http.Client{Timeout: 2 * time.Second}
	response, err := client.Get(url)
	if err != nil {
		t.Fatalf("GET %s error = %v", url, err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	return response.StatusCode, body
}

func serveMux(t *testing.T, mux http.Handler, method, target, contentType, body string) *httptest.ResponseRecorder {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	request := httptest.NewRequest(method, target, reader)
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	recorder := httptest.NewRecorder()
	mux.ServeHTTP(recorder, request)
	return recorder
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func TestDefaultHealthResponse(t *testing.T) {
	_, _, mux := newHumaTestProvider(t, nil, "demo", "1.2.3", nil, nil)

	recorder := serveMux(t, mux, http.MethodGet, "/health", "", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET /health status = %d, want 200", recorder.Code)
	}
	if contentType := recorder.Header().Get("Content-Type"); !strings.HasPrefix(contentType, "application/json") {
		t.Errorf("Content-Type = %q, want application/json", contentType)
	}
	var body map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body %q: %v", recorder.Body.String(), err)
	}
	if body["status"] != "ok" || body["service"] != "demo" || body["version"] != "1.2.3" {
		t.Errorf("body = %v, want default ok/service/version", body)
	}
	if len(body) != 3 {
		t.Errorf("body = %v, want exactly status/service/version", body)
	}
}

type customHealthOutput struct {
	Status int `json:"-"`
	Body   struct {
		Status string `json:"status"`
		Reason string `json:"reason"`
	}
}

func TestUserRegisteredHealthIsPreserved(t *testing.T) {
	prepare := func(api huma.API, _ *http.ServeMux) {
		huma.Register(api, huma.Operation{
			Method:      http.MethodGet,
			Path:        DefaultProbePath,
			OperationID: "customHealth",
			Summary:     "Custom health",
		}, func(_ context.Context, _ *struct{}) (*customHealthOutput, error) {
			output := &customHealthOutput{Status: http.StatusServiceUnavailable}
			output.Body.Status = "degraded"
			output.Body.Reason = "database offline"
			return output, nil
		})
	}
	provider, api, mux := newHumaTestProvider(t, nil, "demo", "1.0.0", prepare, nil)

	if got := api.OpenAPI().Paths[DefaultProbePath].Get.OperationID; got != "customHealth" {
		t.Errorf("health operationId = %q, want customHealth", got)
	}
	paths, _ := provider.spec["paths"].(map[string]any)
	healthPath, _ := paths[DefaultProbePath].(map[string]any)
	healthGet, _ := healthPath["get"].(map[string]any)
	if healthGet["operationId"] != "customHealth" {
		t.Errorf("exported health operationId = %v, want customHealth", healthGet["operationId"])
	}

	recorder := serveMux(t, mux, http.MethodGet, DefaultProbePath, "", "")
	if recorder.Code != http.StatusServiceUnavailable {
		t.Errorf("GET /health status = %d, want 503 from user handler", recorder.Code)
	}
}

func TestUserRegisteredNotifyIsPreserved(t *testing.T) {
	prepare := func(api huma.API, _ *http.ServeMux) {
		huma.Register(api, huma.Operation{
			Method:      http.MethodPost,
			Path:        DefaultNotifyPath,
			OperationID: "customEvents",
		}, func(_ context.Context, _ *struct{}) (*struct{}, error) {
			return &struct{}{}, nil
		})
	}
	_, api, _ := newHumaTestProvider(t, nil, "demo", "1.0.0", prepare, nil)

	if got := api.OpenAPI().Paths[DefaultNotifyPath].Post.OperationID; got != "customEvents" {
		t.Errorf("notify operationId = %q, want customEvents", got)
	}
}

func TestHealthAndNotifyRejectUnexpectedMethods(t *testing.T) {
	_, _, mux := newHumaTestProvider(t, nil, "demo", "1.0.0", nil, nil)

	recorder := serveMux(t, mux, http.MethodPost, DefaultProbePath, "application/json", "{}")
	if recorder.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST /health status = %d, want 405", recorder.Code)
	}

	recorder = serveMux(t, mux, http.MethodGet, DefaultNotifyPath, "", "")
	if recorder.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET notify status = %d, want 405", recorder.Code)
	}
}

func TestNotifyRefreshesBaseURL(t *testing.T) {
	first := newMockGateway(t)
	second := newMockGateway(t)
	provider, _, _ := newHumaTestProvider(t, first, "demo", "1.0.0", nil, nil)
	startProvider(t, provider)
	waitFor(t, 5*time.Second, "registration", func() bool {
		puts, _, _ := first.counts()
		return puts >= 1
	})

	payload := fmt.Sprintf(`{"type":"gateway_restarting","nextBaseUrl":%q}`, second.server.URL)
	response, err := http.Post(
		fmt.Sprintf("http://127.0.0.1:%d%s", provider.Port(), DefaultNotifyPath),
		"application/json",
		strings.NewReader(payload),
	)
	if err != nil {
		t.Fatalf("notify POST error = %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("notify status = %d, want 204", response.StatusCode)
	}

	root := `D:\demo`
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := provider.client.Patch(ctx, "demo", localgateway.PatchOptions{ProjectRoot: &root}); err != nil {
		t.Fatalf("Patch() after notify error = %v", err)
	}
	if provider.client.BaseURL() != second.server.URL {
		t.Errorf("BaseURL() = %q, want %q", provider.client.BaseURL(), second.server.URL)
	}
	if _, patches, _ := second.counts(); patches != 1 {
		t.Errorf("second gateway PATCH calls = %d, want 1", patches)
	}
	if _, patches, _ := first.counts(); patches != 0 {
		t.Errorf("first gateway PATCH calls = %d, want 0", patches)
	}
}

func TestNotifyAcceptsSnakeCaseBaseURL(t *testing.T) {
	first := newMockGateway(t)
	second := newMockGateway(t)
	provider, _, _ := newHumaTestProvider(t, first, "demo", "1.0.0", nil, nil)
	startProvider(t, provider)
	waitFor(t, 5*time.Second, "registration", func() bool {
		puts, _, _ := first.counts()
		return puts >= 1
	})

	payload := fmt.Sprintf(`{"type":"gateway_restarting","next_base_url":%q}`, second.server.URL)
	response, err := http.Post(
		fmt.Sprintf("http://127.0.0.1:%d%s", provider.Port(), DefaultNotifyPath),
		"application/json",
		strings.NewReader(payload),
	)
	if err != nil {
		t.Fatalf("notify POST error = %v", err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("notify status = %d, want 204", response.StatusCode)
	}
	if got := provider.client.BaseURL(); got != second.server.URL {
		t.Errorf("BaseURL() = %q, want %q", got, second.server.URL)
	}
}

func TestNotifyRejectsMalformedPayload(t *testing.T) {
	gateway := newMockGateway(t)
	provider, _, _ := newHumaTestProvider(t, gateway, "demo", "1.0.0", nil, nil)
	startProvider(t, provider)
	waitFor(t, 5*time.Second, "registration", func() bool {
		puts, _, _ := gateway.counts()
		return puts >= 1 && providerRegistered(provider)
	})

	url := fmt.Sprintf("http://127.0.0.1:%d%s", provider.Port(), DefaultNotifyPath)
	for _, payload := range []string{
		`{"type":`,
		`{"type":"gateway_restarting"}`,
		`{"type":"gateway_restarting","nextBaseUrl":"not-a-url"}`,
	} {
		response, err := http.Post(url, "application/json", strings.NewReader(payload))
		if err != nil {
			t.Fatalf("notify POST error = %v", err)
		}
		response.Body.Close()
		if response.StatusCode < 400 || response.StatusCode >= 500 {
			t.Errorf("payload %q status = %d, want 4xx", payload, response.StatusCode)
		}
	}
}

func TestCustomServeAdapter(t *testing.T) {
	gateway := newMockGateway(t)
	mux := http.NewServeMux()
	api := humago.New(mux, huma.DefaultConfig("adapter-demo", "1.0.0"))

	adapterCalled := make(chan struct{}, 1)
	adapterContext := make(chan context.Context, 1)
	serve := func(ctx context.Context, listener net.Listener) error {
		server := &http.Server{Handler: mux}
		go func() { _ = server.Serve(listener) }()
		adapterCalled <- struct{}{}
		adapterContext <- ctx
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return server.Shutdown(shutdownCtx)
	}

	provider, err := New().
		Name("adapter-demo").
		Version("1.0.0").
		API(api).
		Serve(serve).
		GatewayClient(gatewayClient(t, gateway.server.URL)).
		Build()
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	stop := startProvider(t, provider)

	select {
	case <-adapterCalled:
	case <-time.After(5 * time.Second):
		t.Fatal("custom ServeFunc was not called")
	}
	var serveContext context.Context
	select {
	case serveContext = <-adapterContext:
	case <-time.After(5 * time.Second):
		t.Fatal("custom ServeFunc did not receive a context")
	}
	if status, _ := httpGet(t, fmt.Sprintf("http://127.0.0.1:%d/health", provider.Port())); status != http.StatusOK {
		t.Errorf("GET /health over custom adapter = %d", status)
	}
	waitFor(t, 5*time.Second, "registration", func() bool {
		puts, _, _ := gateway.counts()
		return puts >= 1 && providerRegistered(provider)
	})
	if err := stop(); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	select {
	case <-serveContext.Done():
	case <-time.After(2 * time.Second):
		t.Error("custom ServeFunc context was not canceled")
	}
}
