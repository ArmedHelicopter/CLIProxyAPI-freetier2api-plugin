package workbuddy

// 本文件锁定「领奖失败必须让用户看见」。
//
// 历史事故：parseClaimResult 修好之后，调用方 claimGrowthReward 仍吞掉错误返回
// (0, 0)，runGrowthTask 于是回一句「任务已达标」——上游返回 HTML 错误页或改了
// 响应结构时，页面显示成功、实际什么都没领到。领奖有副作用，失败必须上抛。

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"freetier2api-plugin/cpasdk/pluginabi"
	"freetier2api-plugin/internal/core"
	"freetier2api-plugin/internal/hostbridge"
)

// claimHost 是只回一种上游响应的假宿主：任务列表 + 领奖。
type claimHost struct {
	taskBody  string
	claimBody string
}

func installClaimHost(t *testing.T, host claimHost) {
	t.Helper()
	hostbridge.Configure(func(_ string, method string, payload any) (json.RawMessage, error) {
		if method != pluginabi.MethodHostHTTPDo {
			return nil, fmt.Errorf("claim host: unsupported method %s", method)
		}
		raw, errMarshal := json.Marshal(payload)
		if errMarshal != nil {
			return nil, errMarshal
		}
		var req struct {
			Method string `json:"method"`
			URL    string `json:"url"`
		}
		if errDecode := json.Unmarshal(raw, &req); errDecode != nil {
			return nil, errDecode
		}
		body := host.taskBody
		if strings.Contains(req.URL, "/claim") {
			body = host.claimBody
		}
		return json.Marshal(map[string]any{
			"StatusCode": http.StatusOK,
			"Headers":    map[string][]string{"Content-Type": {"application/json"}},
			"Body":       []byte(body),
		})
	})
	t.Cleanup(func() { hostbridge.Configure(nil) })
}

// TestRunGrowthTaskSurfacesClaimFailure 锁定领奖解析失败必须变成任务的错误结果。
func TestRunGrowthTaskSurfacesClaimFailure(t *testing.T) {
	// 任务已达标 → runGrowthTask 直接走领奖分支，不执行自动动作。
	installClaimHost(t, claimHost{
		taskBody:  `{"code":0,"data":[{"task_code":"chat_5","progress":{"current":5,"target":5}}]}`,
		claimBody: `<html><body>upstream error</body></html>`,
	})
	client := NewClient(newRuntime(core.Env{}), Options{Context: context.Background()})
	cred := testGlobalCredential("uid-claim")
	cred.SetRealm(RegionCN)

	_, _, _, errRun := runGrowthTask(context.Background(), client, cred, "chat_5")
	if errRun == nil {
		t.Fatal("领奖响应不可解析时必须报错，不能报成「任务已达标」")
	}
	if !strings.Contains(errRun.Error(), "领奖失败") {
		t.Fatalf("error = %v, want 领奖失败", errRun)
	}
}

// TestRunGrowthTaskReportsClaimedReward 锁定正常路径不受影响。
func TestRunGrowthTaskReportsClaimedReward(t *testing.T) {
	installClaimHost(t, claimHost{
		taskBody:  `{"code":0,"data":[{"task_code":"chat_5","progress":{"current":5,"target":5}}]}`,
		claimBody: `{"code":0,"data":{"credit":12,"energy":3}}`,
	})
	client := NewClient(newRuntime(core.Env{}), Options{Context: context.Background()})
	cred := testGlobalCredential("uid-claim")
	cred.SetRealm(RegionCN)

	message, credit, energy, errRun := runGrowthTask(context.Background(), client, cred, "chat_5")
	if errRun != nil {
		t.Fatalf("正常领奖不应报错: %v", errRun)
	}
	if credit != 12 || energy != 3 {
		t.Fatalf("reward = %d/%d, want 12/3", credit, energy)
	}
	if !strings.Contains(message, "已领奖") {
		t.Fatalf("message = %q, want 已领奖", message)
	}
}
