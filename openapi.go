package provider

import (
	"encoding/json"
	"fmt"

	"github.com/danielgtaylor/huma/v2"
)

// fromHuma 把 api.OpenAPI() 导出为可 JSON 序列化的 map，供注册上报 apiInfo。
func fromHuma(api huma.API) (map[string]any, error) {
	data, err := json.Marshal(api.OpenAPI())
	if err != nil {
		return nil, fmt.Errorf("provider: marshal OpenAPI: %w", err)
	}
	var document map[string]any
	if err := json.Unmarshal(data, &document); err != nil {
		return nil, fmt.Errorf("provider: unmarshal OpenAPI: %w", err)
	}
	return document, nil
}
