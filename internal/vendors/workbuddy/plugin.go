package workbuddy

// 本文件集中 WorkBuddy 适配层需要的「协议层内部能力」：
//   - 上游客户端工厂（配置与状态目录由根层注入，协议层不读宿主配置）；
//   - 额度响应归一（原先在根层 quota.go）；
//   - 会话头族派生（原先在根层 executor.go）；
//   - 推理档位表发布（原先在根层 models.go）；
//   - 任务动作清单来源（任务执行仍由根层编排，注入描述即可）。
//
// 这些都是 WorkBuddy 特有的，不属于 core，也不该留在 ABI 适配层。

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"freetier2api-plugin/cpasdk/pluginapi"
	"freetier2api-plugin/internal/core"
)

var (
	clientFactoryMu sync.RWMutex
	clientFactory   func(context.Context) *Client
)

// SetClientFactory 注入上游客户端工厂（由根层在注册时调用）。
func SetClientFactory(factory func(context.Context) *Client) {
	clientFactoryMu.Lock()
	clientFactory = factory
	clientFactoryMu.Unlock()
}

// UpstreamClient 用注入的工厂构造一个客户端。
//
// 未注入时返回 nil：调用方必须检查（测试与未注册场景）。
func UpstreamClient(ctx context.Context) *Client {
	clientFactoryMu.RLock()
	factory := clientFactory
	clientFactoryMu.RUnlock()
	if factory == nil {
		return nil
	}
	return factory(ctx)
}

// ChatMetaFromBody 从请求体与请求头派生会话头族。
func ChatMetaFromBody(body []byte, headers http.Header) ChatMeta {
	conversationKey := ConversationKey(body)
	turnKey := TurnKey(body)

	// 入站透传的链路 ID 优先（调用方可能已经建好了链路）。
	traceID := ""
	if headers != nil {
		traceID = strings.TrimSpace(headers.Get("X-Trace-ID"))
	}

	conversationRequestID := ""
	switch {
	case conversationKey != "" && turnKey != "":
		conversationRequestID = TurnRequestID(conversationKey + ":" + turnKey)
	case turnKey != "":
		conversationRequestID = TurnRequestID(turnKey)
	case conversationKey != "":
		conversationRequestID = RequestIDForKey(conversationKey)
	default:
		conversationRequestID = TurnRequestID("")
	}

	return ChatMeta{
		ConversationID:        conversationKey,
		ConversationRequestID: conversationRequestID,
		TraceID:               traceID,
	}
}

// PublishEffortTables 把模型清单里的档位信息按区域合并发布给对话路径。
//
// 对话请求构造时要按模型查推理档位（上游对档位校验很严），因此模型清单
// 一拿到就把档位表推给协议层缓存。
func PublishEffortTables(region Region, models []ModelInfo) {
	defaults := make(map[string]string, len(models))
	supported := make(map[string][]string, len(models))
	for _, model := range models {
		if model.DefaultEffort != "" {
			defaults[model.ID] = model.DefaultEffort
		}
		if len(model.Efforts) > 0 {
			supported[model.ID] = model.Efforts
		}
	}
	SetEffortTablesForRegion(region, defaults, supported)
}

// BuildQuotaResponse 把账号额度转成宿主的归一化额度结构。
//
// 结构层次：Summary 放关键数字（宿主的额度卡片直接读它），
// Groups/Buckets 放明细（展开后看）。
func BuildQuotaResponse(credential *Credential, balance *Balance) pluginapi.QuotaFetchResponse {
	region := credential.Realm()
	response := pluginapi.QuotaFetchResponse{
		Subscription: &pluginapi.QuotaSubscription{
			Plan:     string(region),
			TierName: RealmDisplayName(region),
		},
		Summary: []pluginapi.QuotaMetric{
			{Key: "credit_remain", Label: "剩余积分", Value: float64(balance.Remain), Format: "number"},
			{Key: "credit_total", Label: "总额度", Value: float64(balance.Total), Format: "number"},
		},
	}
	if balance.Total > 0 {
		used := balance.Total - balance.Remain
		if used < 0 {
			used = 0
		}
		response.Summary = append(response.Summary,
			pluginapi.QuotaMetric{Key: "credit_used", Label: "已用积分", Value: float64(used), Format: "number"})
	}
	if len(balance.Packages) > 0 {
		buckets := make([]pluginapi.QuotaBucket, 0, len(balance.Packages))
		for _, pkg := range balance.Packages {
			fraction := 0.0
			if pkg.Total > 0 {
				fraction = float64(pkg.Remain) / float64(pkg.Total)
			}
			bucket := pluginapi.QuotaBucket{
				Window:            pkg.Name,
				RemainingFraction: fraction,
				Description:       fmt.Sprintf("%d / %d", pkg.Remain, pkg.Total),
			}
			if pkg.ExpireAt > 0 {
				bucket.ResetTime = FormatUnixTime(pkg.ExpireAt)
			}
			buckets = append(buckets, bucket)
		}
		response.Groups = []pluginapi.QuotaGroup{{
			DisplayName: "资源包明细",
			Buckets:     buckets,
		}}
	}
	return response
}

// RealmDisplayName 返回域的展示名。
func RealmDisplayName(region Region) string {
	if region.IsGlobal() {
		return "国际版 (WorkBuddy AI)"
	}
	return "国内版 (CodeBuddy)"
}

// FormatUnixTime 把 Unix 秒格式化为 RFC3339（无效值返回空串）。
func FormatUnixTime(seconds int64) string {
	if seconds <= 0 {
		return ""
	}
	return time.Unix(seconds, 0).UTC().Format(time.RFC3339)
}

var (
	taskSourceMu sync.RWMutex
	taskSource   func(region Region) []core.Task
)

// SetTaskSource 注入任务动作清单来源（由根层在注册时调用）。
//
// 任务执行需要账户遍历、并发锁与状态记录，属于根层编排职责；这里只取
// 「本区域暴露哪些任务」给控制台展示。
func SetTaskSource(source func(region Region) []core.Task) {
	taskSourceMu.Lock()
	taskSource = source
	taskSourceMu.Unlock()
}

// TaskActions 返回某区域可暴露的任务动作。
func TaskActions(region Region) []core.Task {
	taskSourceMu.RLock()
	source := taskSource
	taskSourceMu.RUnlock()
	if source == nil {
		return nil
	}
	return source(region)
}
