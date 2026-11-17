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

func newTestProvider(t *testing.T, config Config, gateway *mockGateway) *Provider {
	t.Helper()
	if config.GatewayClient == nil {
		config.GatewayClient = gatewayClient(t, gateway.server.URL)
	}
	provider, err := New(config)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return provider
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

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func TestRunRegistersMinimalConfig(t *testing.T) {
	gateway := newMockGateway(t)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /hello", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("hello"))
	})

	provider := newTestProvider(t, Config{
		Name:    "demo",
		Version: "1.2.3",
		Handler: mux,
		Routes: []Route{{
			Method:      "GET",
			Path:        "/hello",
			OperationID: "hello",
			Summary:     "Say hello",
		}},
	}, gateway)
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
		t.Fatalf("apiInfo = %v", request.body["apiInfo"])
	}
	paths, ok := apiInfo["paths"].(map[string]any)
	if !ok {
		t.Fatalf("apiInfo.paths = %v", apiInfo["paths"])
	}
	if _, ok := paths["/health"]; !ok {
		t.Error("generated OpenAPI lacks /health")
	}
	if _, ok := paths["/hello"]; !ok {
		t.Error("generated OpenAPI lacks /hello")
	}

	if status, _ := httpGet(t, fmt.Sprintf("http://127.0.0.1:%d/health", provider.Port())); status != http.StatusOK {
		t.Errorf("GET /health status = %d, want 200", status)
	}
	if status, body := httpGet(t, fmt.Sprintf("http://127.0.0.1:%d/hello", provider.Port())); status != http.StatusOK || string(body) != "hello" {
		t.Errorf("GET /hello = %d %q, want 200 hello", status, body)
	}

	if _, _, deletes := gateway.counts(); deletes != 0 {
		t.Errorf("DELETE calls = %d, want 0", deletes)
	}
}

func TestCustomProbeAndNotifyPaths(t *testing.T) {
	gateway := newMockGateway(t)
	provider := newTestProvider(t, Config{
		Name:       "custom-paths",
		Version:    "0.1.0",
		ProbePath:  "/readyz",
		NotifyPath: "/events/localgateway",
	}, gateway)
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

// serveProviderHandler 直接调用组装好的最终 handler，便于注入 request context。
func serveProviderHandler(t *testing.T, provider *Provider, request *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	recorder := httptest.NewRecorder()
	provider.handler.ServeHTTP(recorder, request)
	return recorder
}

func TestDefaultHealthResponse(t *testing.T) {
	provider, err := New(Config{Name: "demo", Version: "1.2.3"})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	recorder := serveProviderHandler(t, provider, httptest.NewRequest(http.MethodGet, "/health", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET /health status = %d, want 200", recorder.Code)
	}
	if contentType := recorder.Header().Get("Content-Type"); contentType != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", contentType)
	}
	var body map[string]string
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body %q: %v", recorder.Body.String(), err)
	}
	if body["status"] != "ok" || body["service"] != "demo" || body["version"] != "1.2.3" {
		t.Errorf("body = %v, want default ok/service/version", body)
	}
}

func TestCustomHealthHandlerControlsResponse(t *testing.T) {
	gateway := newMockGateway(t)
	provider := newTestProvider(t, Config{
		Name:    "custom-health",
		Version: "1.0.0",
		HealthHandler: func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/problem+json")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"status":"degraded","reason":"database offline"}`))
		},
	}, gateway)
	startProvider(t, provider)
	waitFor(t, 5*time.Second, "registration", func() bool {
		puts, _, _ := gateway.counts()
		return puts >= 1 && providerRegistered(provider)
	})

	url := fmt.Sprintf("http://127.0.0.1:%d/health", provider.Port())
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
	if response.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("GET /health status = %d, want 503", response.StatusCode)
	}
	if contentType := response.Header.Get("Content-Type"); contentType != "application/problem+json" {
		t.Errorf("Content-Type = %q, want application/problem+json", contentType)
	}
	const wantBody = `{"status":"degraded","reason":"database offline"}`
	if string(body) != wantBody {
		t.Errorf("body = %q, want %q", body, wantBody)
	}
}

func TestCustomHealthHandlerReceivesRequestContext(t *testing.T) {
	type contextKey string
	const probeKey contextKey = "probe-context"

	provider, err := New(Config{
		Name:    "ctx-demo",
		Version: "1.0.0",
		HealthHandler: func(w http.ResponseWriter, r *http.Request) {
			value, _ := r.Context().Value(probeKey).(string)
			if value != "from-context" {
				http.Error(w, "context value missing", http.StatusInternalServerError)
				return
			}
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(value))
		},
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	request := httptest.NewRequest(http.MethodGet, "/health", nil)
	request = request.WithContext(context.WithValue(request.Context(), probeKey, "from-context"))
	recorder := serveProviderHandler(t, provider, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET /health status = %d, want 200", recorder.Code)
	}
	if got := recorder.Body.String(); got != "from-context" {
		t.Errorf("body = %q, want the request context value", got)
	}
}

func TestUnregisterOnShutdown(t *testing.T) {
	gateway := newMockGateway(t)
	provider := newTestProvider(t, Config{
		Name:                 "demo",
		Version:              "1.0.0",
		UnregisterOnShutdown: true,
	}, gateway)
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
	provider := newTestProvider(t, Config{
		Name:          "demo",
		Version:       "1.0.0",
		RegisterRetry: 5 * time.Second,
	}, gateway)

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

func TestRegistrationStopsAfterRetryWindow(t *testing.T) {
	gateway := newMockGateway(t)
	gateway.putFailures = 100
	provider := newTestProvider(t, Config{
		Name:          "demo",
		Version:       "1.0.0",
		RegisterRetry: 250 * time.Millisecond,
	}, gateway)

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

func TestNotifyRefreshesBaseURL(t *testing.T) {
	first := newMockGateway(t)
	second := newMockGateway(t)
	provider := newTestProvider(t, Config{
		Name:    "demo",
		Version: "1.0.0",
	}, first)
	startProvider(t, provider)
	waitFor(t, 5*time.Second, "registration", func() bool {
		puts, _, _ := first.counts()
		return puts >= 1
	})

	payload := fmt.Sprintf(`{"type":"gateway_restarting","nextBaseUrl":%q}`, second.server.URL)
	response, err := http.Post(
		fmt.Sprintf("http://127.0.0.1:%d/_localgateway/events", provider.Port()),
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

func TestNotifyRejectsMalformedPayload(t *testing.T) {
	gateway := newMockGateway(t)
	provider := newTestProvider(t, Config{Name: "demo", Version: "1.0.0"}, gateway)
	startProvider(t, provider)
	waitFor(t, 5*time.Second, "registration", func() bool {
		puts, _, _ := gateway.counts()
		return puts >= 1 && providerRegistered(provider)
	})

	url := fmt.Sprintf("http://127.0.0.1:%d/_localgateway/events", provider.Port())
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
		if response.StatusCode != http.StatusBadRequest {
			t.Errorf("payload %q status = %d, want 400", payload, response.StatusCode)
		}
	}
}

func TestGeneratedOpenAPIFromRoutes(t *testing.T) {
	gateway := newMockGateway(t)
	provider := newTestProvider(t, Config{
		Name:    "openapi-demo",
		Version: "2.0.0",
		Routes: []Route{
			{
				Method:      "get",
				Path:        "/echo",
				OperationID: "echo",
				Summary:     "Echo back",
				Tags:        []string{"demo"},
				Responses: map[string]any{
					"200": map[string]any{"description": "echo"},
					"400": "bad request",
				},
			},
			{
				Method:      "POST",
				Path:        "/echo",
				OperationID: "echoPost",
				RequestBody: map[string]any{"type": "object", "properties": map[string]any{"text": map[string]any{"type": "string"}}},
				Responses:   map[string]any{"201": map[string]any{"description": "created"}},
			},
		},
	}, gateway)
	startProvider(t, provider)
	waitFor(t, 5*time.Second, "registration", func() bool {
		puts, _, _ := gateway.counts()
		return puts >= 1 && providerRegistered(provider)
	})

	request, _ := gateway.lastRequest(http.MethodPut)
	apiInfo := request.body["apiInfo"].(map[string]any)
	if apiInfo["openapi"] != "3.1.0" {
		t.Errorf("openapi = %v", apiInfo["openapi"])
	}
	info := apiInfo["info"].(map[string]any)
	if info["title"] != "openapi-demo" || info["version"] != "2.0.0" {
		t.Errorf("info = %v", info)
	}
	paths := apiInfo["paths"].(map[string]any)
	healthPath := paths["/health"].(map[string]any)
	healthGet := healthPath["get"].(map[string]any)
	if healthGet["operationId"] != "getHealth" {
		t.Errorf("health operationId = %v", healthGet["operationId"])
	}
	echo := paths["/echo"].(map[string]any)
	get := echo["get"].(map[string]any)
	if get["operationId"] != "echo" || get["summary"] != "Echo back" {
		t.Errorf("GET /echo = %v", get)
	}
	if tags, ok := get["tags"].([]any); !ok || len(tags) != 1 || tags[0] != "demo" {
		t.Errorf("GET /echo tags = %v", get["tags"])
	}
	responses := get["responses"].(map[string]any)
	if responses["200"].(map[string]any)["description"] != "echo" {
		t.Errorf("200 response = %v", responses["200"])
	}
	if responses["400"].(map[string]any)["description"] != "bad request" {
		t.Errorf("400 response = %v", responses["400"])
	}
	post := echo["post"].(map[string]any)
	requestBody := post["requestBody"].(map[string]any)
	content := requestBody["content"].(map[string]any)
	schema := content["application/json"].(map[string]any)["schema"].(map[string]any)
	if schema["type"] != "object" {
		t.Errorf("request schema = %v", schema)
	}
}

func TestRawOpenAPIIsPreservedAndExtended(t *testing.T) {
	gateway := newMockGateway(t)
	raw := map[string]any{
		"openapi": "3.0.3",
		"info":    map[string]any{"title": "custom title", "version": "9.9.9"},
		"paths": map[string]any{
			"/custom": map[string]any{
				"get": map[string]any{
					"operationId": "customGet",
					"responses":   map[string]any{"204": map[string]any{"description": "done"}},
				},
			},
		},
	}
	provider := newTestProvider(t, Config{
		Name:    "raw-demo",
		Version: "1.0.0",
		OpenAPI: raw,
		Routes:  []Route{{Method: "GET", Path: "/ignored"}},
	}, gateway)
	startProvider(t, provider)
	waitFor(t, 5*time.Second, "registration", func() bool {
		puts, _, _ := gateway.counts()
		return puts >= 1 && providerRegistered(provider)
	})

	request, _ := gateway.lastRequest(http.MethodPut)
	apiInfo := request.body["apiInfo"].(map[string]any)
	if apiInfo["openapi"] != "3.0.3" {
		t.Errorf("openapi = %v", apiInfo["openapi"])
	}
	info := apiInfo["info"].(map[string]any)
	if info["title"] != "custom title" || info["version"] != "9.9.9" {
		t.Errorf("info overwritten: %v", info)
	}
	paths := apiInfo["paths"].(map[string]any)
	if _, ok := paths["/custom"]; !ok {
		t.Error("raw /custom path was dropped")
	}
	if _, ok := paths["/health"]; !ok {
		t.Error("raw document lacks the added /health path")
	}
	if _, ok := paths["/ignored"]; ok {
		t.Error("raw OpenAPI must take precedence over declared routes")
	}
}

func TestConfigValidation(t *testing.T) {
	valid := func() Config {
		return Config{Name: "demo", Version: "1.0.0"}
	}

	cases := []struct {
		name   string
		mutate func(*Config)
	}{
		{name: "empty name", mutate: func(c *Config) { c.Name = " " }},
		{name: "empty version", mutate: func(c *Config) { c.Version = "" }},
		{name: "negative port", mutate: func(c *Config) { c.Port = -1 }},
		{name: "port too large", mutate: func(c *Config) { c.Port = 65536 }},
		{name: "relative probe path", mutate: func(c *Config) { c.ProbePath = "health" }},
		{name: "root probe path", mutate: func(c *Config) { c.ProbePath = "/" }},
		{name: "relative notify path", mutate: func(c *Config) { c.NotifyPath = "events" }},
		{name: "trailing slash notify path", mutate: func(c *Config) { c.NotifyPath = "/events/" }},
		{name: "same built-in paths", mutate: func(c *Config) { c.ProbePath = "/probe"; c.NotifyPath = "/probe" }},
		{name: "negative probe timeout", mutate: func(c *Config) { c.ProbeTimeoutMs = -1 }},
		{name: "negative register retry", mutate: func(c *Config) { c.RegisterRetry = -time.Second }},
		{name: "route without method", mutate: func(c *Config) { c.Routes = []Route{{Path: "/x"}} }},
		{name: "route without path", mutate: func(c *Config) { c.Routes = []Route{{Method: "GET"}} }},
		{name: "route bad path", mutate: func(c *Config) { c.Routes = []Route{{Method: "GET", Path: "x"}} }},
		{name: "route bad method", mutate: func(c *Config) { c.Routes = []Route{{Method: "FETCH", Path: "/x"}} }},
		{name: "duplicate route", mutate: func(c *Config) {
			c.Routes = []Route{{Method: "GET", Path: "/x"}, {Method: "get", Path: "/x"}}
		}},
		{name: "duplicate operation id", mutate: func(c *Config) {
			c.Routes = []Route{
				{Method: "GET", Path: "/x", OperationID: "same"},
				{Method: "GET", Path: "/y", OperationID: "same"},
			}
		}},
		{name: "invalid response value", mutate: func(c *Config) {
			c.Routes = []Route{{Method: "GET", Path: "/x", Responses: map[string]any{"200": 42}}}
		}},
		{name: "openapi not an object", mutate: func(c *Config) { c.OpenAPI = []int{1} }},
		{name: "openapi null", mutate: func(c *Config) { c.OpenAPI = (*int)(nil) }},
		{name: "openapi version not string", mutate: func(c *Config) { c.OpenAPI = map[string]any{"openapi": 3.1} }},
		{name: "openapi unsupported version", mutate: func(c *Config) { c.OpenAPI = map[string]any{"openapi": "2.0"} }},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			config := valid()
			testCase.mutate(&config)
			if _, err := New(config); !errors.Is(err, ErrInvalidConfig) {
				t.Fatalf("New() error = %v, want ErrInvalidConfig", err)
			}
		})
	}
}

func TestBusinessHandlerConflictFailsAtNew(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {})
	if _, err := New(Config{Name: "demo", Version: "1.0.0", Handler: mux}); !errors.Is(err, ErrRouteConflict) {
		t.Fatalf("New() error = %v, want ErrRouteConflict", err)
	}

	notifyMux := http.NewServeMux()
	notifyMux.HandleFunc("/_localgateway/events", func(w http.ResponseWriter, _ *http.Request) {})
	if _, err := New(Config{Name: "demo", Version: "1.0.0", Handler: notifyMux}); !errors.Is(err, ErrRouteConflict) {
		t.Fatalf("notify conflict error = %v, want ErrRouteConflict", err)
	}

	catchAll := http.NewServeMux()
	catchAll.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {})
	if _, err := New(Config{Name: "demo", Version: "1.0.0", Handler: catchAll}); !errors.Is(err, ErrRouteConflict) {
		t.Fatalf("catch-all conflict error = %v, want ErrRouteConflict", err)
	}
}

func TestCustomServeAdapter(t *testing.T) {
	gateway := newMockGateway(t)
	adapterCalled := make(chan struct{}, 1)
	adapterContext := make(chan context.Context, 1)
	serve := func(ctx context.Context, listener net.Listener, handler http.Handler) error {
		server := &http.Server{Handler: handler}
		go func() { _ = server.Serve(listener) }()
		adapterCalled <- struct{}{}
		adapterContext <- ctx
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return server.Shutdown(shutdownCtx)
	}

	provider := newTestProvider(t, Config{
		Name:    "adapter-demo",
		Version: "1.0.0",
		Serve:   serve,
	}, gateway)
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

func TestHealthAndNotifyRejectUnexpectedMethods(t *testing.T) {
	gateway := newMockGateway(t)
	provider := newTestProvider(t, Config{Name: "demo", Version: "1.0.0"}, gateway)
	startProvider(t, provider)
	waitFor(t, 5*time.Second, "registration", func() bool {
		puts, _, _ := gateway.counts()
		return puts >= 1 && providerRegistered(provider)
	})

	url := fmt.Sprintf("http://127.0.0.1:%d/health", provider.Port())
	request, _ := http.NewRequest(http.MethodPost, url, strings.NewReader("{}"))
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("POST /health error = %v", err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("POST /health status = %d, want 405", response.StatusCode)
	}

	notifyURL := fmt.Sprintf("http://127.0.0.1:%d/_localgateway/events", provider.Port())
	response, err = http.Get(notifyURL)
	if err != nil {
		t.Fatalf("GET notify error = %v", err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET notify status = %d, want 405", response.StatusCode)
	}
}
