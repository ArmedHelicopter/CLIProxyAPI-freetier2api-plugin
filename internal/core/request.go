package core

// 本文件提供与供应商无关的请求解析助手。

import (
	"encoding/json"
	"net/http"
	"strings"
)

// ExtractClientIP 从请求头里取客户端 IP（X-Forwarded-For 优先）。
func ExtractClientIP(header http.Header) string {
	if header == nil {
		return ""
	}
	if forwarded := strings.TrimSpace(header.Get("X-Forwarded-For")); forwarded != "" {
		if idx := strings.IndexByte(forwarded, ','); idx >= 0 {
			forwarded = forwarded[:idx]
		}
		return strings.TrimSpace(forwarded)
	}
	return strings.TrimSpace(header.Get("X-Real-IP"))
}

// HasImagePart 报告 chat-completions 请求体里是否包含图片输入。
func HasImagePart(body []byte) bool {
	if len(body) == 0 {
		return false
	}
	var payload map[string]any
	if errUnmarshal := json.Unmarshal(body, &payload); errUnmarshal != nil {
		return false
	}
	messages, okMessages := payload["messages"].([]any)
	if !okMessages {
		return false
	}
	for _, item := range messages {
		message, okMessage := item.(map[string]any)
		if !okMessage {
			continue
		}
		parts, okParts := message["content"].([]any)
		if !okParts {
			continue
		}
		for _, part := range parts {
			partMap, okPartMap := part.(map[string]any)
			if !okPartMap {
				continue
			}
			if partType, okType := partMap["type"].(string); okType && partType == "image_url" {
				return true
			}
		}
	}
	return false
}
