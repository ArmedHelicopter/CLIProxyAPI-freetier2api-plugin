package admin

// 本文件是任务引擎的管理接口适配层：解析请求、调用 tasks.Service、封装响应。

import (
	"encoding/json"
	"net/http"

	"freetier2api-plugin/cpasdk/pluginapi"
	"freetier2api-plugin/internal/core"
	"freetier2api-plugin/internal/hostbridge"
)

func (s *Service) handleTasksScan(req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	ctx, cancel := hostbridge.ManagementContext(req, requestTimeout)
	defer cancel()
	return JSONResponse(http.StatusOK, s.tasks.Scan(ctx, hostbridge.RequestCallbackID(req)))
}

func (s *Service) handleTasksRun(req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	body, status := s.tasks.StartQueue(req.Body, hostbridge.RequestCallbackID(req))
	return JSONResponse(status, body)
}

func (s *Service) handleTasksQueue(_ pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	return JSONResponse(http.StatusOK, s.tasks.QueueSnapshot())
}

func (s *Service) handleTasksAuto(req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	var body struct {
		AuthID    string `json:"auth_id"`
		UID       string `json:"uid"`
		AccountID string `json:"account_id"`
		TaskCode  string `json:"task_code"`
	}
	if len(req.Body) > 0 {
		_ = json.Unmarshal(req.Body, &body)
	}
	ctx, cancel := hostbridge.ManagementContext(req, requestTimeout)
	defer cancel()
	identifier := core.FirstNonEmpty(body.AuthID, body.UID, body.AccountID)
	result, status := s.tasks.RunAuto(ctx, hostbridge.RequestCallbackID(req), identifier, body.TaskCode)
	return JSONResponse(status, result)
}

func (s *Service) handleTasksAutoAll(req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	ctx, cancel := hostbridge.ManagementContext(req, requestTimeout)
	defer cancel()
	return JSONResponse(http.StatusOK, s.tasks.RunAutoAll(ctx, hostbridge.RequestCallbackID(req)))
}

func (s *Service) handleSchoolStatus(req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	ctx, cancel := hostbridge.ManagementContext(req, requestTimeout)
	defer cancel()
	return JSONResponse(http.StatusOK, s.tasks.ActivityStatus(ctx, hostbridge.RequestCallbackID(req)))
}
