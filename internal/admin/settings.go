package admin

import (
	"net/http"
	"slices"
	"strconv"
	"strings"

	"freetier2api-plugin/cpasdk/pluginapi"
	"freetier2api-plugin/internal/core"
	"freetier2api-plugin/internal/state"
	"freetier2api-plugin/internal/tasks"
)

type settingsUpdateRequest struct {
	AutoCheckin    *bool             `json:"auto_checkin"`
	AutoCheckinAt  string            `json:"auto_checkin_at"`
	AutoTasks      *bool             `json:"auto_tasks"`
	VendorSettings map[string]string `json:"vendor_settings"`
	TaskSchedules  map[string][]int  `json:"task_schedules"`

	// Deprecated: 兼容旧版页面/脚本的固定字段。
	CheckinHours   []int `json:"checkin_hours"`
	TravelHours    []int `json:"travel_hours"`
	ActivityHours  []int `json:"activity_hours"`
	KeepaliveHours []int `json:"keepalive_hours"`
	BlackcatHours  []int `json:"blackcat_hours"`
}

func (s *Service) handleSettingsRequest(req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	body, errDecode := decodeRequest[settingsUpdateRequest](req.Body)
	if errDecode != nil {
		return BadJSONRequestResponse()
	}
	vendorSettings, errVendor := s.normalizeVendorSettings(body.VendorSettings)
	if errVendor != nil {
		return ErrorResponse(errVendor)
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
		if len(vendorSettings) > 0 {
			if current.VendorSettings == nil {
				current.VendorSettings = map[string]string{}
			}
			for key, value := range vendorSettings {
				// 空串表示撤销运行期覆盖，重新由 YAML 配置决定。
				if value == "" {
					delete(current.VendorSettings, key)
					continue
				}
				current.VendorSettings[key] = value
			}
			changed["vendor_settings"] = current.VendorSettings
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

// normalizeVendorSettings 校验页面提交的供应商运行期设置。
//
// 键必须由某个供应商通过 core.RuntimeFieldsProvider 自己声明，枚举值必须落在
// 该字段的 EnumValues 内：页面按同一份声明渲染控件，后端只接受声明过的键，
// 避免写进一份谁都不会读的配置。空串表示撤销覆盖（该键从 state 中删除）。
func (s *Service) normalizeVendorSettings(input map[string]string) (map[string]string, error) {
	if len(input) == 0 {
		return nil, nil
	}
	fields := map[string]pluginapi.ConfigField{}
	for _, item := range s.runtimeFields() {
		fields[item.Field.Name] = item.Field
	}
	out := make(map[string]string, len(input))
	for key, value := range input {
		field, okField := fields[key]
		if !okField {
			return nil, core.NewPluginError("vendor_setting_unknown",
				"未知的供应商运行期设置："+key, http.StatusBadRequest)
		}
		value = strings.TrimSpace(value)
		if value == "" {
			out[key] = ""
			continue
		}
		switch field.Type {
		case pluginapi.ConfigFieldTypeEnum:
			if !slices.Contains(field.EnumValues, value) {
				return nil, core.NewPluginError("vendor_setting_invalid",
					"设置 "+key+" 的取值 "+value+" 不被支持，可选："+strings.Join(field.EnumValues, " / "),
					http.StatusBadRequest)
			}
		case pluginapi.ConfigFieldTypeBoolean:
			parsed, okBool := parseStrictBool(value)
			if !okBool {
				return nil, core.NewPluginError("vendor_setting_invalid",
					"设置 "+key+" 需要布尔值（true/false），收到 "+value, http.StatusBadRequest)
			}
			value = strconv.FormatBool(parsed)
		}
		out[key] = value
	}
	return out, nil
}

// vendorField 是「某个供应商自己声明的运行期字段」。
//
// 字段声明归供应商所有（core.RuntimeFieldsProvider）：过滤规则、取值集合都写在
// 各自 adapter 里，管理端只负责汇总、校验与按页面契约输出，不认任何供应商专属键。
type vendorField struct {
	Vendor string
	Field  pluginapi.ConfigField
}

// runtimeFields 汇总各供应商声明的运行期字段。
//
// 同名键只保留一次：一家供应商的多个实例（workbuddycn / workbuddyglobal）会声明
// 同一批键，而键本身是插件级配置，重复渲染只会让页面出现两个互不相干的控件。
func (s *Service) runtimeFields() []vendorField {
	seen := map[string]bool{}
	var out []vendorField
	for _, vendor := range s.registry.Vendors() {
		provider, okProvider := core.As[core.RuntimeFieldsProvider](vendor)
		if !okProvider {
			continue
		}
		for _, field := range provider.RuntimeFields() {
			if field.Name == "" || seen[field.Name] {
				continue
			}
			seen[field.Name] = true
			out = append(out, vendorField{Vendor: vendor.Name(), Field: field})
		}
	}
	return out
}

// vendorFieldView 是控制台读取的运行期字段形状（snake_case，与页面脚本一致）。
//
// pluginapi.ConfigField 是宿主 ABI 的结构（字段名大写、不带 tag），直接塞进
// /status 会让页面读到 undefined；页面契约在这里显式定义。
type vendorFieldView struct {
	Name        string   `json:"name"`
	Type        string   `json:"type"`
	EnumValues  []string `json:"enum_values,omitempty"`
	Description string   `json:"description"`
	Vendor      string   `json:"vendor"`
}

// settingsView 是 /status 的 settings 载荷：任务引擎的可读设置 + 供应商字段。
func (s *Service) settingsView() map[string]any {
	settings := s.tasks.EffectiveSettings()
	fields := s.runtimeFields()
	views := make([]vendorFieldView, 0, len(fields))
	for _, item := range fields {
		views = append(views, vendorFieldView{
			Name:        item.Field.Name,
			Type:        string(item.Field.Type),
			EnumValues:  item.Field.EnumValues,
			Description: item.Field.Description,
			Vendor:      item.Vendor,
		})
	}
	settings["fields"] = views
	return settings
}

// handleScheduledRun 手动触发一个定时任务。
func (s *Service) handleScheduledRun(_ pluginapi.ManagementRequest, kind string) pluginapi.ManagementResponse {
	s.tasks.RunAsync(kind)
	return JSONResponse(http.StatusOK, map[string]any{
		"ok": true, "started": true, "task": kind,
		"message": "任务已在后台开始执行，进度见插件日志",
	})
}
