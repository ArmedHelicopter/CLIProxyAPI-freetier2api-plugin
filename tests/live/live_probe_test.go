//go:build live

package live

// 手动实况探针：MINIMAX_LIVE=1 且设置 MINIMAX_LIVE_CREDS 时运行。
// 只打印截断后的响应，不打印凭证内容。

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"freetier2api-plugin/cpasdk/pluginabi"
	"freetier2api-plugin/internal/core"
	"freetier2api-plugin/internal/hostabi"
	"freetier2api-plugin/internal/vendors/minimaxcode"
)

func liveCredential(t *testing.T) *minimaxcode.Credential {
	t.Helper()
	if os.Getenv("MINIMAX_LIVE") == "" {
		t.Skip("MINIMAX_LIVE 未设置，跳过实况探针")
	}
	path := os.Getenv("MINIMAX_LIVE_CREDS")
	if path == "" {
		t.Skip("MINIMAX_LIVE_CREDS 未设置")
	}
	raw, errRead := os.ReadFile(path)
	if errRead != nil {
		t.Fatalf("read creds: %v", errRead)
	}
	cred, errParse := minimaxcode.ParseCredential(raw, minimaxcode.RegionCN)
	if errParse != nil {
		t.Fatalf("parse creds: %v", errParse)
	}
	configureDirectHost(t)
	return cred
}

func configureDirectHost(t *testing.T) {
	t.Helper()
	hostabi.Configure(func(callbackID, method string, payload any) (json.RawMessage, error) {
		if method != pluginabi.MethodHostHTTPDo {
			return nil, fmt.Errorf("probe host: unsupported method %s", method)
		}
		var req struct {
			Method  string              `json:"method"`
			URL     string              `json:"url"`
			Headers map[string][]string `json:"headers"`
			Body    []byte              `json:"body"`
		}
		rawPayload, errMarshal := json.Marshal(payload)
		if errMarshal != nil {
			return nil, errMarshal
		}
		if errDecode := json.Unmarshal(rawPayload, &req); errDecode != nil {
			return nil, errDecode
		}
		var bodyReader io.Reader
		if len(req.Body) > 0 {
			bodyReader = strings.NewReader(string(req.Body))
		}
		httpReq, errNew := http.NewRequest(req.Method, req.URL, bodyReader)
		if errNew != nil {
			return nil, errNew
		}
		for k, vs := range req.Headers {
			for _, v := range vs {
				httpReq.Header.Add(k, v)
			}
		}
		client := &http.Client{Timeout: 110 * time.Second}
		resp, errDo := client.Do(httpReq)
		if errDo != nil {
			return nil, errDo
		}
		defer func() { _ = resp.Body.Close() }()
		respBody, errRead := io.ReadAll(resp.Body)
		if errRead != nil {
			return nil, errRead
		}
		return json.Marshal(map[string]any{"StatusCode": resp.StatusCode, "Headers": resp.Header, "Body": respBody})
	})
	t.Cleanup(func() { hostabi.Configure(nil) })
}

func runStreamProbe(t *testing.T, cred *minimaxcode.Credential, payload []byte) (text string, chunks int) {
	t.Helper()
	var builder strings.Builder
	errStream := minimaxcode.ChatStream(context.Background(), cred, &core.ExecuteRequest{Payload: payload}, "", func(chunk []byte) error {
		chunks++
		if chunks <= 20 {
			t.Logf("CHUNK[%d] %s", chunks, truncateProbe(string(chunk), 500))
		}
		builder.Write(chunk)
		return nil
	})
	if errStream != nil {
		t.Fatalf("ChatStream: %v", errStream)
	}
	return builder.String(), chunks
}

func truncateProbe(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + fmt.Sprintf("...(len=%d)", len(s))
}

func TestLiveProbeA(t *testing.T) {
	cred := liveCredential(t)
	content := "[系统指令] You are Claude Code, Anthropic's official CLI tool for Claude.\n\n 用户：1. 测试子代理和tool"
	payload, _ := json.Marshal(map[string]any{
		"model":    "minimax-m3.1-flash",
		"messages": []any{map[string]any{"role": "user", "content": content}},
		"stream":   true,
	})
	started := time.Now()
	text, chunks := runStreamProbe(t, cred, payload)
	t.Logf("耗时 %s, chunks=%d, 文本 len=%d", time.Since(started), chunks, len(text))
	t.Logf("响应: %s", truncateProbe(text, 2000))
}

func TestLiveProbeE(t *testing.T) {
	cred := liveCredential(t)
	payload, _ := json.Marshal(map[string]any{
		"model":    "minimax-m3.1-flash",
		"messages": []any{map[string]any{"role": "system", "content": "be brief"}, map[string]any{"role": "user", "content": "查一下北京天气"}},
		"stream":   true,
		"tools": []any{map[string]any{"type": "function", "function": map[string]any{
			"name": "get_weather", "description": "查询指定城市的当前天气",
			"parameters": map[string]any{"type": "object", "properties": map[string]any{"city": map[string]any{"type": "string"}}, "required": []any{"city"}},
		}}},
	})
	resp, errChat := minimaxcode.Chat(context.Background(), cred, &core.ExecuteRequest{Payload: payload}, "")
	if errChat != nil {
		t.Logf("Chat 返回错误: %v", errChat)
		return
	}
	t.Logf("Chat 响应: %s", truncateProbe(string(resp), 3000))
}

func TestLiveProbeD(t *testing.T) {
	cred := liveCredential(t)
	quota, errQuota := minimaxcode.FetchQuota(context.Background(), cred, "")
	if errQuota != nil {
		t.Fatalf("FetchQuota: %v", errQuota)
	}
	encoded, _ := json.Marshal(quota)
	t.Logf("quota: %s", string(encoded))
}

func TestLiveProbeC(t *testing.T) {
	cred := liveCredential(t)
	payload, _ := json.Marshal(map[string]any{
		"model": "minimax-agent", "messages": []any{map[string]any{"role": "user", "content": "用一句话回答：1+1等于几？"}}, "stream": true,
	})
	started := time.Now()
	text, chunks := runStreamProbe(t, cred, payload)
	t.Logf("耗时 %s, chunks=%d, 文本 len=%d", time.Since(started), chunks, len(text))
	t.Logf("响应: %s", truncateProbe(text, 2000))
}

func TestLiveProbeB(t *testing.T) {
	cred := liveCredential(t)
	content := strings.Join([]string{
		"[系统指令] 你是一个通过文本协议调用工具的助手。",
		"[工具]", `[{"name":"get_weather","description":"查询指定城市的当前天气","parameters":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}}]`,
		"[工具调用格式]", `<tool_call>`, `{"name":"get_weather","arguments":{"city":"北京"}}`, `</tool_call>`,
		" 用户：北京今天天气怎么样？",
	}, "\n")
	payload, _ := json.Marshal(map[string]any{
		"model": "minimax-m3.1-flash", "messages": []any{map[string]any{"role": "user", "content": content}}, "stream": true,
	})
	started := time.Now()
	text, chunks := runStreamProbe(t, cred, payload)
	t.Logf("耗时 %s, chunks=%d, 文本 len=%d", time.Since(started), chunks, len(text))
	t.Logf("响应: %s", truncateProbe(text, 3000))
}
