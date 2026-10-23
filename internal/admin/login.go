package admin

// 本文件实现「粘贴 localhost 授权回调链接」的登录兜底入口。
//
// Trae / CodeArts 的上游只认 127.0.0.1 回调。插件与浏览器不在同一台机器时（远程
// 主机、Docker、WSL），那条回调永远到不了，登录会一直卡在「等待授权中」。
// 控制台把本机回调地址明示给用户，用户在浏览器完成授权后把地址栏里的整条链接
// 粘回来，插件就地解析完成登录。

import (
	"net/http"

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
	body, errDecode := decodeRequest[loginCallbackRequest](req.Body)
	if errDecode != nil {
		// 页面按 AuthLoginStatus 契约读 status / message，这里不能套用通用的
		// {ok, error} 形状，否则「请刷新页面后重试」这句提示会丢失。
		return JSONResponse(http.StatusBadRequest, map[string]any{
			"ok": false, "status": string(pluginapi.AuthLoginStatusError),
			"message": "请求体解析失败，请刷新页面后重试",
		})
	}
	// 入参校验（state 非空、URL 非空）统一由 App.SubmitLoginCallback 负责：
	// 它是唯一调用供应商的地方，校验放在这里就得再抄一份。
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

	if status == pluginapi.AuthLoginStatusSuccess {
		// 粘贴路径的凭证只回到**页面**，没人替它落盘——正常轮询路径是宿主在
		// auth.login.poll 之后写文件的，这里没有那个宿主环节。不补这一下，页面
		// 显示「登录成功」而 auth 目录里什么都没有。
		if errSave := saveAuthFile(hostbridge.RequestCallbackID(req), result.Auth.FileName, result.Auth.StorageJSON); errSave != nil {
			payload["ok"] = false
			payload["status"] = string(pluginapi.AuthLoginStatusError)
			payload["message"] = "登录已完成，但保存凭证失败：" + errSave.Error()
			logger.Error("login callback persist failed (state=%s): %v", body.State, errSave)
			return JSONResponse(http.StatusOK, payload)
		}
		payload["auth_id"] = result.Auth.FileName
		payload["label"] = result.Auth.Label
	}

	// 落盘成功时 saveAuthFile 已经记过一笔，这里只补非成功分支的审计行。
	if status != pluginapi.AuthLoginStatusSuccess {
		logger.Info("login callback handled (state=%s status=%s)", body.State, status)
	}
	return JSONResponse(http.StatusOK, payload)
}
