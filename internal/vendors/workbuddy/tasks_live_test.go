//go:build live

package workbuddy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"freetier2api-plugin/cpasdk/pluginabi"
	"freetier2api-plugin/internal/core"
	"freetier2api-plugin/internal/hostbridge"
)

// 国际版每日活跃奖励的实况探针（-tags live）。
//
// 运行方式：
//
//	WORKBUDDY_LIVE=1 WORKBUDDY_LIVE_CREDS=<auth.json 路径> ;
//	  go test -tags live ./internal/vendors/workbuddy/ -run TestLiveGlobalDailyBonus -v
//
// 只打印截断后的非敏感信息（日期、分数、点亮天数），不打印凭证内容。

func configureWorkBuddyProbeHost(t *testing.T) {
	t.Helper()
	hostbridge.Configure(func(callbackID, method string, payload any) (json.RawMessage, error) {
		if method != pluginabi.MethodHostHTTPDo {
			return nil, fmt.Errorf("probe host: unsupported method %s", method)
		}
		var req struct {
			Method  string              `json:"method"`
			URL     string              `json:"url"`
			Headers map[string][]string `json:"headers"`
			Body    []byte              `json:"body"`
		}
		rawPayload, errMarshal := json.Marshal(payload)
		if errMarshal != nil {
			return nil, errMarshal
		}
		if errDecode := json.Unmarshal(rawPayload, &req); errDecode != nil {
			return nil, errDecode
		}
		var bodyReader io.Reader
		if len(req.Body) > 0 {
			bodyReader = strings.NewReader(string(req.Body))
		}
		httpReq, errNew := http.NewRequest(req.Method, req.URL, bodyReader)
		if errNew != nil {
			return nil, errNew
		}
		for k, vs := range req.Headers {
			for _, v := range vs {
				httpReq.Header.Add(k, v)
			}
		}
		client := &http.Client{Timeout: 60 * time.Second}
		resp, errDo := client.Do(httpReq)
		if errDo != nil {
			return nil, errDo
		}
		defer func() { _ = resp.Body.Close() }()
		respBody, errRead := io.ReadAll(resp.Body)
		if errRead != nil {
			return nil, errRead
		}
		return json.Marshal(map[string]any{"StatusCode": resp.StatusCode, "Headers": resp.Header, "Body": respBody})
	})
	t.Cleanup(func() { hostbridge.Configure(nil) })
}

func TestLiveGlobalDailyBonus(t *testing.T) {
	if os.Getenv("WORKBUDDY_LIVE") == "" {
		t.Skip("WORKBUDDY_LIVE 未设置，跳过实况探针")
	}
	path := os.Getenv("WORKBUDDY_LIVE_CREDS")
	if path == "" {
		t.Skip("WORKBUDDY_LIVE_CREDS 未设置")
	}
	raw, errRead := os.ReadFile(path)
	if errRead != nil {
		t.Fatalf("read creds: %v", errRead)
	}
	cred, errParse := ParseCredential(raw, RegionGlobal)
	if errParse != nil {
		t.Fatalf("parse creds: %v", errParse)
	}
	if !cred.Realm().IsGlobal() {
		t.Fatalf("expected global credential, got %q", cred.Realm())
	}
	configureWorkBuddyProbeHost(t)

	rt := newRuntime(core.Env{})
	client := NewClient(rt, rt.options(context.Background()))

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	today, lit, errHeat := client.GrowthHeatmapToday(ctx, cred)
	if errHeat != nil {
		t.Fatalf("heatmap: %v", errHeat)
	}
	t.Logf("heatmap: today=%s score=%d lit_days=%d", today.Date, today.Score, len(lit))

	result, errBonus := client.GlobalDailyBonus(ctx, cred)
	if errBonus != nil {
		t.Fatalf("daily bonus: %v", errBonus)
	}
	t.Logf("daily bonus: already=%v streak=%d", result.Already, result.Streak)

	// 幂等复核：点亮后再跑一次应命中 Already（热力图回读滞后时除外，
	// 那种情况日志里会看到第二次仍上报一次，与排程多次兜底的语义一致）。
	again, errAgain := client.GlobalDailyBonus(ctx, cred)
	if errAgain != nil {
		t.Fatalf("daily bonus again: %v", errAgain)
	}
	t.Logf("daily bonus again: already=%v streak=%d", again.Already, again.Streak)
}
