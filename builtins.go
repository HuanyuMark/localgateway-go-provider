package provider

import (
	"context"
	"fmt"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
)

// 内建端点的 OperationID；用户不应重复使用。
const (
	healthOperationID = "getHealth"
	notifyOperationID = "gatewayEvent"
)

// healthOutput 用 map 承载响应，避免 Huma 的 schema-link transformer 在结构体
// 响应中追加 $schema 字段，保证默认响应严格为 status/service/version。
type healthOutput struct {
	Body map[string]string
}

type notifyInput struct {
	Body notifyPayload
}

type notifyPayload struct {
	Type        string `json:"type" doc:"Event type"`
	NextBaseURL string `json:"nextBaseUrl,omitempty" doc:"New gateway base URL"`
}

type notifyOutput struct{}

// registerBuiltins 在用户尚未注册同路径同方法时补上内建端点。
//
// huma.Register 对重复 OperationID 会 panic；这里用 recover 转成 ErrInvalidConfig。
func (p *Provider) registerBuiltins(api huma.API) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf(
				"%w: register built-in endpoints: %v (do not reuse operationId %q or %q)",
				ErrInvalidConfig, recovered, healthOperationID, notifyOperationID,
			)
		}
	}()

	paths := api.OpenAPI().Paths
	if paths[p.cfg.ProbePath] == nil || paths[p.cfg.ProbePath].Get == nil {
		huma.Register(api, huma.Operation{
			Method:      http.MethodGet,
			Path:        p.cfg.ProbePath,
			OperationID: healthOperationID,
			Summary:     "Liveness probe",
		}, p.healthHandler)
	}
	if paths[p.cfg.NotifyPath] == nil || paths[p.cfg.NotifyPath].Post == nil {
		huma.Register(api, huma.Operation{
			Method:      http.MethodPost,
			Path:        p.cfg.NotifyPath,
			OperationID: notifyOperationID,
			Summary:     "Gateway lifecycle event",
		}, p.notifyHandler)
	}
	return nil
}

func (p *Provider) healthHandler(_ context.Context, _ *struct{}) (*healthOutput, error) {
	return &healthOutput{Body: map[string]string{
		"status":  "ok",
		"service": p.cfg.Name,
		"version": p.cfg.Version,
	}}, nil
}

func (p *Provider) notifyHandler(_ context.Context, input *notifyInput) (*notifyOutput, error) {
	event := input.Body
	if event.Type != "gateway_restarting" {
		// 其它事件由调用方按需处理；库只消费重启预通知。
		return &notifyOutput{}, nil
	}

	nextBaseURL := event.NextBaseURL
	if nextBaseURL == "" {
		return nil, huma.Error400BadRequest("gateway_restarting requires nextBaseUrl")
	}

	client := p.gatewayClient()
	if client == nil {
		return nil, huma.Error503ServiceUnavailable("provider is not started")
	}
	if err := client.SetBaseURL(nextBaseURL); err != nil {
		return nil, huma.Error400BadRequest("nextBaseUrl is not a valid http(s) URL")
	}
	return &notifyOutput{}, nil
}
