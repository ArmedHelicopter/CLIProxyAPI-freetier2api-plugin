package admin

// 本文件实现「粘贴 localhost 授权回调链接」的登录兜底入口。
//
// Trae / CodeArts 的上游只认 127.0.0.1 回调。插件与浏览器不在同一台机器时（远程
// 主机、Docker、WSL），那条回调永远到不了，登录会一直卡在「等待授权中」。
// 控制台把本机回调地址明示给用户，用户在浏览器完成授权后把地址栏里的整条链接
// 粘回来，插件就地解析完成登录。

import (
	"encoding/json"
	"net/http"
	"strings"

	"freetier2api-plugin/cpasdk/pluginapi"
	"freetier2api-plugin/internal/hostbridge"
	"freetier2api-plugin/internal/logger"
)

// loginCallbackRequest 是一次回调粘贴请求。
type loginCallbackRequest struct {
	State string `json:"state"`
	URL   string `json:"url"`
}

// handleLoginCallbackRequest 处理用户粘回来的 localhost 回调链接。
//
// 业务失败（会话过期、链接不匹配、上游还没准备好）一律 200 + ok=false 回给
// 页面：页面要的是一句能直接显示的原因，而不是状态码——错误文案由供应商侧
// 生成，已经刻意去掉了回调链接本身（里面带 refreshToken / 票据密钥）。
func (s *Service) handleLoginCallbackRequest(req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	var body loginCallbackRequest
	if len(req.Body) > 0 {
		if errUnmarshal := json.Unmarshal(req.Body, &body); errUnmarshal != nil {
			return JSONResponse(http.StatusBadRequest, map[string]any{
				"ok": false, "status": string(pluginapi.AuthLoginStatusError),
				"message": "请求体解析失败，请刷新页面后重试",
			})
		}
	}
	if strings.TrimSpace(body.State) == "" {
		return JSONResponse(http.StatusOK, map[string]any{
			"ok": false, "status": string(pluginapi.AuthLoginStatusError),
			"message": "登录会话已丢失，请重新发起登录",
		})
	}

	ctx, cancel := hostbridge.ManagementContext(req, requestTimeout)
	defer cancel()

	result, errSubmit := s.deps.SubmitLoginCallback(ctx, body.State, body.URL)
	if errSubmit != nil {
		return JSONResponse(http.StatusOK, map[string]any{
			"ok": false, "status": string(pluginapi.AuthLoginStatusError),
			"message": errSubmit.Error(),
		})
	}

	status := result.Status
	payload := map[string]any{
		"ok":      status == pluginapi.AuthLoginStatusSuccess,
		"status":  string(status),
		"message": result.Message,
	}
	// CodeArts：票据还没换到凭证时，用户还需要在新标签页打开华为云登录页。
	if result.RedirectURL != "" {
		payload["redirect_url"] = result.RedirectURL
	}
	logger.Info("login callback submitted (state=%s status=%s)", body.State, status)
	return JSONResponse(http.StatusOK, payload)
}
