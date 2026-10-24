package autoclaw

// 本文件给登录链路的 HTTP 单测提供 loopback 桩：hostbridge.Client 默认经宿主
// 出站，测试里换成直连 httptest 的转发器（与 codearts 包的 useLoopbackHost 同款）。
//
// 请求/响应都用 map 解析，避免在测试代码里再抄一份宿主的 JSON 字段名；
// 字段口径见 internal/hostbridge/http.go 的 rpcHostHTTPRequest/Response。

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"freetier2api-plugin/cpasdk/pluginabi"
	"freetier2api-plugin/internal/hostbridge"
)

func useLoopbackHost(t *testing.T) {
	t.Helper()
	hostbridge.Configure(func(callbackID, method string, payload any) (json.RawMessage, error) {
		if method != pluginabi.MethodHostHTTPDo {
			return nil, nil
		}
		rawPayload, errMarshal := json.Marshal(payload)
		if errMarshal != nil {
			return nil, errMarshal
		}
		var req map[string]any
		if errDecode := json.Unmarshal(rawPayload, &req); errDecode != nil {
			return nil, errDecode
		}
		methodText, _ := req["method"].(string)
		urlText, _ := req["url"].(string)
		var bodyReader io.Reader
		if encoded, okEncoded := req["body"].(string); okEncoded && encoded != "" {
			body, errBody := base64.StdEncoding.DecodeString(encoded)
			if errBody != nil {
				return nil, errBody
			}
			bodyReader = strings.NewReader(string(body))
		}
		httpReq, errNew := http.NewRequest(methodText, urlText, bodyReader)
		if errNew != nil {
			return nil, errNew
		}
		if rawHeaders, okHeaders := req["headers"].(map[string]any); okHeaders {
			for key, rawValues := range rawHeaders {
				values, _ := rawValues.([]any)
				for _, rawValue := range values {
					if value, okValue := rawValue.(string); okValue {
						httpReq.Header.Add(key, value)
					}
				}
			}
		}
		resp, errDo := http.DefaultClient.Do(httpReq)
		if errDo != nil {
			return nil, errDo
		}
		defer func() { _ = resp.Body.Close() }()
		body, errRead := io.ReadAll(resp.Body)
		if errRead != nil {
			return nil, errRead
		}
		wrapped, errWrap := json.Marshal(map[string]any{
			"status_code": resp.StatusCode,
			"headers":     resp.Header,
			"body":        body,
		})
		if errWrap != nil {
			return nil, errWrap
		}
		return wrapped, nil
	})
}
