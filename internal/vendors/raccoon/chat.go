package raccoon

// 本文件实现小浣熊的 chat-completions 转发。
//
// 上游就是 OpenAI 形状：请求体原样转发（model 由适配器保证是目录内规范
// id），返回 OpenAI JSON / SSE，插件原样透传。

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"freetier2api-plugin/internal/core"
	"freetier2api-plugin/internal/hostbridge"
)

// chatHTTPTimeout 是非流式对话的上限。
const chatHTTPTimeout = 10 * time.Minute

// ChatRequest 是一次对话转发的输入。
type ChatRequest struct {
	// Token 是出站凭证（JWT access token）。
	Token string
	// Body 是宿主翻译好的 chat-completions 请求体。
	Body []byte
}

// Chat 执行一次非流式对话，返回上游的完整响应体。
func Chat(ctx context.Context, llmBase string, req ChatRequest) ([]byte, error) {
	resp, errDo := doChat(ctx, llmBase, req)
	if errDo != nil {
		return nil, errDo
	}
	defer func() { _ = resp.Body.Close() }()

	body, errRead := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if errRead != nil {
		return nil, &core.Failure{Status: resp.StatusCode, Msg: "read chat response: " + errRead.Error(), Kind: core.FailureKindTransient}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, Classify(resp.StatusCode, string(body))
	}
	// 业务信封（{"code","message"}）：code != 0 按业务错误处理。
	if code, message, ok := envelopeError(body); ok {
		return nil, ClassifyBusiness(resp.StatusCode, code, message)
	}
	return body, nil
}

// ChatStream 执行一次流式对话，返回上游响应体（调用方负责关闭）。
func ChatStream(ctx context.Context, llmBase string, req ChatRequest) (*http.Response, error) {
	endpoint := llmBase + ChatPath
	httpReq, errReq := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(req.Body))
	if errReq != nil {
		return nil, fmt.Errorf("build chat stream request: %w", errReq)
	}
	ApplyChatHeaders(httpReq, req.Token)

	resp, errDo := hostbridge.StreamClient(ctx, 0).Do(httpReq)
	if errDo != nil {
		return nil, &core.Failure{Msg: "chat stream: " + errDo.Error(), Kind: core.FailureKindTransient}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		defer func() { _ = resp.Body.Close() }()
		payload, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return nil, Classify(resp.StatusCode, string(payload))
	}
	return resp, nil
}

// doChat 发起一次非流式对话请求。
func doChat(ctx context.Context, llmBase string, req ChatRequest) (*http.Response, error) {
	endpoint := llmBase + ChatPath
	httpReq, errReq := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(req.Body))
	if errReq != nil {
		return nil, fmt.Errorf("build chat request: %w", errReq)
	}
	ApplyChatHeaders(httpReq, req.Token)

	resp, errDo := hostbridge.Client(ctx, chatHTTPTimeout).Do(httpReq)
	if errDo != nil {
		return nil, &core.Failure{Msg: "chat: " + errDo.Error(), Kind: core.FailureKindTransient}
	}
	return resp, nil
}

// envelopeError 检查业务信封 {"code","message"}：code 存在且非 0 时返回 (code, message, true)。
func envelopeError(body []byte) (int64, string, bool) {
	var probe struct {
		Code    *int64 `json:"code"`
		Message string `json:"message"`
		Msg     string `json:"msg"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return 0, "", false
	}
	if probe.Code == nil || *probe.Code == 0 {
		return 0, "", false
	}
	message := probe.Message
	if message == "" {
		message = probe.Msg
	}
	return *probe.Code, message, true
}
