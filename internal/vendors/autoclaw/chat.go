package autoclaw

// 本文件实现 AutoClaw 的 chat-completions 转发。
//
// 通道：region 基址下的 /autoclaw-proxy/proxy/autoclaw/v1/chat/completions
// （Agent2API 的 chat_proxy 通道，模型经 X-Request-Model 头声明）。
// 上游返回 OpenAI 兼容的 JSON / SSE，插件原样透传。
//
// 说明：Agent2API 还有「沙箱 relay」通道（wss 设备上线 + agent/send +
// /api/events），依赖 per-account 云端沙箱与 WebSocket 长连接。本插件按
// 「凭证驻留宿主进程、按请求出站」的形态选择 proxy 通道；relay 通道需要
// 的常驻会话状态与插件模型不符，暂不实现（凭证里保留 sandbox 字段以备扩展）。

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"freetier2api-plugin/internal/core"
	"freetier2api-plugin/internal/hostbridge"
)

// chatHTTPTimeout 是非流式对话的上限。
const chatHTTPTimeout = 10 * time.Minute

// ChatRequest 是一次对话转发的输入。
type ChatRequest struct {
	// Token 是出站凭证（access token）。
	Token string
	// Body 是宿主翻译好的 chat-completions 请求体。
	Body []byte
	// Model 是剥掉前缀后的裸模型名。
	Model string
}

// Chat 执行一次非流式对话，返回上游的完整响应体。
func Chat(ctx context.Context, baseURL string, req ChatRequest) ([]byte, error) {
	resp, errDo := doChat(ctx, baseURL, req)
	if errDo != nil {
		return nil, errDo
	}
	defer func() { _ = resp.Body.Close() }()

	body, errRead := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if errRead != nil {
		return nil, &core.Failure{Status: resp.StatusCode, Msg: "read chat response: " + errRead.Error(), Kind: core.FailureKindTransient}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, Classify(resp.StatusCode, 0, string(body))
	}
	// 业务信封形态（{"code","msg","data"}）：非 0 业务码按错误处理。
	if code, msg, ok := envelopeError(body); ok {
		return nil, Classify(resp.StatusCode, code, msg)
	}
	return body, nil
}

// ChatStream 执行一次流式对话，返回上游响应体（调用方负责关闭）。
func ChatStream(ctx context.Context, baseURL string, req ChatRequest) (*http.Response, error) {
	endpoint := baseURL + ChatPath
	body := prepareBody(req.Body, req.Model)

	httpReq, errReq := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if errReq != nil {
		return nil, fmt.Errorf("build chat stream request: %w", errReq)
	}
	ApplyChatHeaders(httpReq, req.Token, req.Model)

	resp, errDo := hostbridge.StreamClient(ctx, 0).Do(httpReq)
	if errDo != nil {
		return nil, &core.Failure{Msg: "chat stream: " + errDo.Error(), Kind: core.FailureKindTransient}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		defer func() { _ = resp.Body.Close() }()
		payload, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return nil, Classify(resp.StatusCode, 0, string(payload))
	}
	return resp, nil
}

// doChat 发起一次非流式对话请求。
func doChat(ctx context.Context, baseURL string, req ChatRequest) (*http.Response, error) {
	endpoint := baseURL + ChatPath
	body := prepareBody(req.Body, req.Model)

	httpReq, errReq := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if errReq != nil {
		return nil, fmt.Errorf("build chat request: %w", errReq)
	}
	ApplyChatHeaders(httpReq, req.Token, req.Model)

	resp, errDo := hostbridge.Client(ctx, chatHTTPTimeout).Do(httpReq)
	if errDo != nil {
		return nil, &core.Failure{Msg: "chat: " + errDo.Error(), Kind: core.FailureKindTransient}
	}
	return resp, nil
}

// prepareBody 是请求体出站前的钩子：把 model 改写为上游路由名并去掉 user 字段
// （与 Agent2API 的 _prepare_proxy_body 一致；user 字段会被上游拒绝）。
func prepareBody(body []byte, model string) []byte {
	if len(body) == 0 {
		return body
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return body
	}
	if strings.TrimSpace(model) != "" {
		payload["model"] = model
	}
	delete(payload, "user")
	encoded, errMarshal := json.Marshal(payload)
	if errMarshal != nil {
		return body
	}
	return encoded
}

// envelopeError 检查业务信封 {"code","msg"}：code 存在且非 0 时返回 (code, msg, true)。
func envelopeError(body []byte) (int64, string, bool) {
	var probe struct {
		Code    *int64 `json:"code"`
		Msg     string `json:"msg"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return 0, "", false
	}
	if probe.Code == nil || *probe.Code == 0 {
		return 0, "", false
	}
	msg := probe.Msg
	if msg == "" {
		msg = probe.Message
	}
	return *probe.Code, msg, true
}
