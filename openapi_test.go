package provider

import (
	"context"
	"net/http"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"
)

type echoOutput struct {
	Body struct {
		Message string `json:"message"`
	}
}

func TestFromHumaExportsSpec(t *testing.T) {
	mux := http.NewServeMux()
	api := humago.New(mux, huma.DefaultConfig("svc", "1.0.0"))
	huma.Register(api, huma.Operation{
		Method:      http.MethodGet,
		Path:        "/echo",
		OperationID: "echo",
		Summary:     "Echo back",
	}, func(_ context.Context, _ *struct{}) (*echoOutput, error) {
		return &echoOutput{}, nil
	})

	document, err := fromHuma(api)
	if err != nil {
		t.Fatalf("fromHuma() error = %v", err)
	}
	if document["openapi"] != "3.1.0" {
		t.Errorf("openapi = %v, want 3.1.0", document["openapi"])
	}
	info, ok := document["info"].(map[string]any)
	if !ok || info["title"] != "svc" || info["version"] != "1.0.0" {
		t.Errorf("info = %v", document["info"])
	}
	paths, ok := document["paths"].(map[string]any)
	if !ok {
		t.Fatalf("paths = %v", document["paths"])
	}
	echoPath, ok := paths["/echo"].(map[string]any)
	if !ok {
		t.Fatalf("paths[/echo] = %v", paths["/echo"])
	}
	get, ok := echoPath["get"].(map[string]any)
	if !ok || get["operationId"] != "echo" || get["summary"] != "Echo back" {
		t.Errorf("GET /echo = %v", echoPath["get"])
	}
}

func TestHumaRawOpenAPI(t *testing.T) {
	config := huma.DefaultConfig("ignored", "0.0.0")
	config.OpenAPI = &huma.OpenAPI{
		OpenAPI: "3.0.3",
		Info:    &huma.Info{Title: "custom title", Version: "9.9.9"},
		Paths: map[string]*huma.PathItem{
			"/custom": {
				Get: &huma.Operation{
					OperationID: "customGet",
					Responses: map[string]*huma.Response{
						"204": {Description: "done"},
					},
				},
			},
		},
	}

	mux := http.NewServeMux()
	api := humago.New(mux, config)
	document, err := fromHuma(api)
	if err != nil {
		t.Fatalf("fromHuma() error = %v", err)
	}

	if document["openapi"] != "3.0.3" {
		t.Errorf("openapi = %v, want 3.0.3", document["openapi"])
	}
	info, ok := document["info"].(map[string]any)
	if !ok || info["title"] != "custom title" || info["version"] != "9.9.9" {
		t.Errorf("info = %v", document["info"])
	}
	paths, ok := document["paths"].(map[string]any)
	if !ok {
		t.Fatalf("paths = %v", document["paths"])
	}
	if _, ok := paths["/custom"]; !ok {
		t.Error("raw /custom path was dropped")
	}
}
