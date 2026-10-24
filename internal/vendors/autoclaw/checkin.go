package autoclaw

// 本文件实现 AutoClaw 的每日签到（POST {base}/autoclaw-proxy/proxy/autoclaw-task-complete）。
//
// 协议依据 Agent2API check.py：body {"task_id":"daily_signin"}，业务信封
// code=0 成功；data.already_completed=true 表示今日已签（幂等），
// data.reward_points 是本次奖励积分。

import (
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

// checkinTaskID 是每日签到任务的任务 ID。
const checkinTaskID = "daily_signin"

// CheckinResult 是一次签到的结果。
type CheckinResult struct {
	Already bool
	Points  int64
	TaskID  string
}

// Checkin 执行一次每日签到。
func Checkin(ctx context.Context, baseURL string, cred *Credential) (*CheckinResult, error) {
	if strings.TrimSpace(baseURL) == "" {
		return nil, fmt.Errorf("upstream base URL is not configured")
	}
	body, errMarshal := json.Marshal(map[string]any{"task_id": checkinTaskID})
	if errMarshal != nil {
		return nil, fmt.Errorf("marshal checkin request: %w", errMarshal)
	}
	endpoint := baseURL + CheckinPath
	req, errReq := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(string(body)))
	if errReq != nil {
		return nil, fmt.Errorf("build checkin request: %w", errReq)
	}
	ApplyUserAPIHeaders(req, cred.AccessTokenValue())

	resp, errDo := hostbridge.Client(ctx, 20*time.Second).Do(req)
	if errDo != nil {
		return nil, &core.Failure{Msg: "checkin: " + errDo.Error(), Kind: core.FailureKindTransient}
	}
	defer func() { _ = resp.Body.Close() }()

	raw, errRead := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if errRead != nil {
		return nil, &core.Failure{Status: resp.StatusCode, Msg: "read checkin response: " + errRead.Error(), Kind: core.FailureKindTransient}
	}
	var envelope struct {
		Code *int64 `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			AlreadyCompleted bool   `json:"already_completed"`
			RewardPoints     any    `json:"reward_points"`
			TaskID           string `json:"task_id"`
		} `json:"data"`
	}
	if errUnmarshal := json.Unmarshal(raw, &envelope); errUnmarshal != nil {
		return nil, Classify(resp.StatusCode, 0, string(raw))
	}
	// code=0 成功；code 缺失且 data 非空也算成功（与桌面端 completeClientTask 一致）。
	ok := (envelope.Code != nil && *envelope.Code == 0) ||
		(envelope.Code == nil && (envelope.Data.AlreadyCompleted || envelope.Data.TaskID != ""))
	if !ok && resp.StatusCode >= 400 {
		return nil, Classify(resp.StatusCode, int64Value(envelope.Code), string(raw))
	}
	if !ok {
		return nil, Classify(resp.StatusCode, int64Value(envelope.Code), envelope.Msg)
	}
	return &CheckinResult{
		Already: envelope.Data.AlreadyCompleted,
		Points:  int64(core.IntFromAny(envelope.Data.RewardPoints)),
		TaskID:  checkinTaskID,
	}, nil
}
