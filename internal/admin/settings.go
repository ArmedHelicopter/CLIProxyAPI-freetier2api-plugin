package admin

import (
	"encoding/json"
	"net/http"
	"strings"

	"freetier2api-plugin/cpasdk/pluginapi"
	"freetier2api-plugin/internal/pluginconfig"
	"freetier2api-plugin/internal/state"
)

type settingsUpdateRequest struct {
	AutoCheckin    *bool  `json:"auto_checkin"`
	AutoCheckinAt  string `json:"auto_checkin_at"`
	AutoTasks      *bool  `json:"auto_tasks"`
	PromptMode     string `json:"prompt_mode"`
	CheckinHours   []int  `json:"checkin_hours"`
	TravelHours    []int  `json:"travel_hours"`
	ActivityHours  []int  `json:"activity_hours"`
	KeepaliveHours []int  `json:"keepalive_hours"`
	BlackcatHours  []int  `json:"blackcat_hours"`
}

func handleSettingsRequest(req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	var body settingsUpdateRequest
	if len(req.Body) > 0 {
		_ = json.Unmarshal(req.Body, &body)
	}
	changed := map[string]any{}
	deps.MutateState(func(current *state.State) {
		if body.AutoCheckin != nil {
			current.AutoCheckin = body.AutoCheckin
			changed["auto_checkin"] = *body.AutoCheckin
		}
		if at := strings.TrimSpace(body.AutoCheckinAt); at != "" {
			if _, _, errClock := pluginconfig.ParseClock(at); errClock == nil {
				current.AutoCheckinAt = at
				changed["auto_checkin_at"] = at
			}
		}
		if body.AutoTasks != nil {
			current.AutoTasks = body.AutoTasks
			changed["auto_tasks"] = *body.AutoTasks
		}
		if mode := strings.ToLower(strings.TrimSpace(body.PromptMode)); mode != "" {
			switch mode {
			case pluginconfig.PromptModePassThrough, pluginconfig.PromptModeCustom, pluginconfig.PromptModeAppend:
				modeCopy := mode
				current.PromptMode = &modeCopy
				changed["prompt_mode"] = mode
			}
		}
		for key, pair := range map[string]struct {
			hours []int
			dest  *[]int
		}{
			"checkin_hours":   {body.CheckinHours, &current.CheckinHours},
			"travel_hours":    {body.TravelHours, &current.TravelHours},
			"activity_hours":  {body.ActivityHours, &current.ActivityHours},
			"keepalive_hours": {body.KeepaliveHours, &current.KeepaliveHours},
			"blackcat_hours":  {body.BlackcatHours, &current.BlackcatHours},
		} {
			if pair.hours != nil {
				*pair.dest = pair.hours
				changed[key] = pair.hours
			}
		}
	})
	deps.PokeScheduler()
	return JSONResponse(http.StatusOK, map[string]any{
		"ok": true, "changed": changed, "settings": deps.EffectiveSettings(),
	})
}

// handleScheduledRun 手动触发一个定时任务。
func handleScheduledRun(_ pluginapi.ManagementRequest, kind string) pluginapi.ManagementResponse {
	go deps.RunScheduled(kind)
	return JSONResponse(http.StatusOK, map[string]any{
		"ok": true, "started": true, "task": kind,
		"message": "任务已在后台开始执行，进度见插件日志",
	})
}
