package provider

import (
	"encoding/json"
	"fmt"
	"strings"
)

// OpenAPI 版本固定为 3.1.0；原始文档若自带受支持的 3.0.x / 3.1.x 版本则保留。
const defaultOpenAPIVersion = "3.1.0"

// buildOpenAPI 组装要注册的 OpenAPI 文档。
//
// OpenAPI 字段存在时优先使用原始文档，只补齐缺失的 info 与探活路径；
// 否则由 Routes 声明生成 spec，并始终保证探活路径有合法文档。
func buildOpenAPI(config Config, routes []Route) (map[string]any, error) {
	if config.OpenAPI != nil {
		return mergeRawOpenAPI(config.OpenAPI, config.Name, config.Version, config.ProbePath)
	}

	paths := make(map[string]any, len(routes)+1)
	for _, route := range routes {
		item, ok := paths[route.Path].(map[string]any)
		if !ok {
			item = map[string]any{}
			paths[route.Path] = item
		}
		item[strings.ToLower(route.Method)] = operationForRoute(route)
	}
	if _, ok := paths[config.ProbePath]; !ok {
		paths[config.ProbePath] = map[string]any{"get": healthOperation()}
	}

	return map[string]any{
		"openapi": defaultOpenAPIVersion,
		"info": map[string]any{
			"title":   config.Name,
			"version": config.Version,
		},
		"paths": paths,
	}, nil
}

func mergeRawOpenAPI(raw any, name, version, probePath string) (map[string]any, error) {
	data, err := json.Marshal(raw)
	if err != nil {
		return nil, fmt.Errorf("%w: OpenAPI is not JSON-serializable: %v", ErrInvalidConfig, err)
	}
	var document map[string]any
	if err := json.Unmarshal(data, &document); err != nil {
		return nil, fmt.Errorf("%w: OpenAPI must be a JSON object: %v", ErrInvalidConfig, err)
	}
	if document == nil {
		return nil, fmt.Errorf("%w: OpenAPI must be a JSON object", ErrInvalidConfig)
	}

	if rawVersion, ok := document["openapi"]; ok {
		versionText, ok := rawVersion.(string)
		if !ok {
			return nil, fmt.Errorf("%w: OpenAPI openapi field must be a string", ErrInvalidConfig)
		}
		if !supportedOpenAPIVersion(versionText) {
			return nil, fmt.Errorf("%w: unsupported OpenAPI version %q; expected 3.0.x or 3.1.x", ErrInvalidConfig, versionText)
		}
	} else {
		document["openapi"] = defaultOpenAPIVersion
	}

	info, err := stringMapField(document, "info")
	if err != nil {
		return nil, err
	}
	if text, _ := info["title"].(string); strings.TrimSpace(text) == "" {
		info["title"] = name
	}
	if text, _ := info["version"].(string); strings.TrimSpace(text) == "" {
		info["version"] = version
	}
	document["info"] = info

	paths, err := stringMapField(document, "paths")
	if err != nil {
		return nil, err
	}
	if _, ok := paths[probePath]; !ok {
		paths[probePath] = map[string]any{"get": healthOperation()}
	}
	document["paths"] = paths

	return document, nil
}

// stringMapField 读取可选的 JSON 对象字段；缺失时返回空对象。
func stringMapField(document map[string]any, field string) (map[string]any, error) {
	value, ok := document[field]
	if !ok || value == nil {
		return map[string]any{}, nil
	}
	object, ok := value.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%w: OpenAPI %s must be a JSON object", ErrInvalidConfig, field)
	}
	return object, nil
}

func operationForRoute(route Route) map[string]any {
	operation := make(map[string]any, 5)
	if route.OperationID != "" {
		operation["operationId"] = route.OperationID
	}
	if route.Summary != "" {
		operation["summary"] = route.Summary
	}
	if len(route.Tags) > 0 {
		operation["tags"] = append([]string(nil), route.Tags...)
	}
	if route.RequestBody != nil {
		operation["requestBody"] = map[string]any{
			"content": map[string]any{
				"application/json": map[string]any{"schema": route.RequestBody},
			},
		}
	}

	responses := make(map[string]any, len(route.Responses))
	if len(route.Responses) == 0 {
		responses["200"] = map[string]any{"description": "OK"}
	}
	for status, value := range route.Responses {
		switch typed := value.(type) {
		case string:
			responses[status] = map[string]any{"description": typed}
		case map[string]any:
			responses[status] = typed
		default:
			// 校验阶段已经拒绝该类型。
			responses[status] = map[string]any{"description": fmt.Sprint(typed)}
		}
	}
	operation["responses"] = responses

	return operation
}

func healthOperation() map[string]any {
	return map[string]any{
		"operationId": "getHealth",
		"summary":     "Liveness probe",
		"responses": map[string]any{
			"200": map[string]any{"description": "Service is healthy"},
		},
	}
}

func supportedOpenAPIVersion(version string) bool {
	for _, prefix := range []string{"3.0.", "3.1."} {
		if rest, ok := strings.CutPrefix(version, prefix); ok {
			if rest == "" {
				return false
			}
			for _, character := range rest {
				if character < '0' || character > '9' {
					return false
				}
			}
			return true
		}
	}
	return false
}
