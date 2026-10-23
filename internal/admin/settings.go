package admin

import (
	"net/http"
	"strings"

	"freetier2api-plugin/cpasdk/pluginapi"
	"freetier2api-plugin/internal/state"
	"freetier2api-plugin/internal/tasks"
)

type settingsUpdateRequest struct {
	AutoCheckin         *bool            `json:"auto_checkin"`
	AutoCheckinAt       string           `json:"auto_checkin_at"`
	AutoTasks           *bool            `json:"auto_tasks"`
	WorkbuddyPromptMode string           `json:"workbuddy_prompt_mode"`
	TaskSchedules       map[string][]int `json:"task_schedules"`

	// Deprecated: 兼容旧版页面/脚本的固定字段。
	CheckinHours   []int `json:"checkin_hours"`
	TravelHours    []int `json:"travel_hours"`
	ActivityHours  []int `json:"activity_hours"`
	KeepaliveHours []int `json:"keepalive_hours"`
	BlackcatHours  []int `json:"blackcat_hours"`
}

func (s *Service) handleSettingsRequest(req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	var body settingsUpdateRequest
	if errDecode := decodeJSONBody(req.Body, &body); errDecode != nil {
		return BadJSONRequestResponse()
	}
	changed := map[string]any{}
	s.deps.MutateState(func(current *state.State) {
		if body.AutoCheckin != nil {
			current.AutoCheckin = body.AutoCheckin
			changed["auto_checkin"] = *body.AutoCheckin
		}
		if at := strings.TrimSpace(body.AutoCheckinAt); at != "" {
			if _, _, errClock := ParseClock(at); errClock == nil {
				current.AutoCheckinAt = at
				changed["auto_checkin_at"] = at
			}
		}
		if body.AutoTasks != nil {
			current.AutoTasks = body.AutoTasks
			changed["auto_tasks"] = *body.AutoTasks
		}
		if mode := strings.ToLower(strings.TrimSpace(body.WorkbuddyPromptMode)); mode != "" {
			switch mode {
			case "passthrough", "custom", "append":
				if current.VendorSettings == nil {
					current.VendorSettings = map[string]string{}
				}
				current.VendorSettings["workbuddy_prompt_mode"] = mode
				changed["workbuddy_prompt_mode"] = mode
			}
		}
		if current.TaskSchedules == nil {
			current.TaskSchedules = map[string][]int{}
		}
		for kind, hours := range body.TaskSchedules {
			kind = strings.TrimSpace(kind)
			if kind == "" {
				continue
			}
			if _, known := s.tasks.Catalog().SpecByKind(kind); !known {
				continue
			}
			current.TaskSchedules[kind] = tasks.NormalizeHours(hours)
			changed["task_schedules"] = current.TaskSchedules
		}
		for kind, hours := range map[string][]int{
			"checkin":   body.CheckinHours,
			"travel":    body.TravelHours,
			"activity":  body.ActivityHours,
			"keepalive": body.KeepaliveHours,
			"blackcat":  body.BlackcatHours,
		} {
			if hours != nil {
				current.TaskSchedules[kind] = tasks.NormalizeHours(hours)
				changed["task_schedules"] = current.TaskSchedules
			}
		}
	})
	if errReconfigure := s.deps.Reconfigure(); errReconfigure != nil {
		return ErrorResponse(errReconfigure)
	}
	s.tasks.Poke()
	return JSONResponse(http.StatusOK, map[string]any{
		"ok": true, "changed": changed, "settings": s.tasks.EffectiveSettings(),
	})
}

// handleScheduledRun 手动触发一个定时任务。
func (s *Service) handleScheduledRun(_ pluginapi.ManagementRequest, kind string) pluginapi.ManagementResponse {
	s.tasks.RunAsync(kind)
	return JSONResponse(http.StatusOK, map[string]any{
		"ok": true, "started": true, "task": kind,
		"message": "任务已在后台开始执行，进度见插件日志",
	})
}
