package codearts

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"freetier2api-plugin/internal/hostbridge"
)

// stubHostHTTPDo 把宿主 HTTP 桥换成「记录请求 + 回空 JSON」的桩。
func stubHostHTTPDo(t *testing.T) **http.Request {
	t.Helper()
	return stubHostWithBody(t, `{}`)
}

// stubHostWithBody 与 stubHostHTTPDo 相同，但回指定的响应体。
func stubHostWithBody(t *testing.T, responseBody string) **http.Request {
	t.Helper()
	var captured *http.Request
	hostbridge.Configure(func(callbackID, method string, payload any) (json.RawMessage, error) {
		raw, errMarshal := json.Marshal(payload)
		if errMarshal != nil {
			return nil, errMarshal
		}
		var req struct {
			Method  string              `json:"method"`
			URL     string              `json:"url"`
			Headers map[string][]string `json:"headers"`
		}
		if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
			return nil, errUnmarshal
		}
		httpReq, errNew := http.NewRequest(req.Method, req.URL, nil)
		if errNew != nil {
			return nil, errNew
		}
		for key, values := range req.Headers {
			for _, value := range values {
				httpReq.Header.Add(key, value)
			}
		}
		captured = httpReq
		return json.Marshal(map[string]any{
			"StatusCode": 200,
			"Headers":    map[string][]string{"Content-Type": {"application/json"}},
			"Body":       []byte(responseBody),
		})
	})
	t.Cleanup(func() { hostbridge.Configure(nil) })
	return &captured
}

// TestChatRequestSendsBenefitChannelHeader 锁定福利通道的开关。
//
// 上游用 maas_type 区分「每日免费额度」与「订阅积分」：带 benefit 才走免费额度。
// 这个头还必须**在签名之前**设置，否则签名覆盖不到它，等于没带。
func TestChatRequestSendsBenefitChannelHeader(t *testing.T) {
	captured := stubHostHTTPDo(t)

	_, errChat := Chat(context.Background(), "", ChatRequest{
		Credential: &Credential{AccessKeyID: "AK", SecretAccessKey: "SK", SecurityToken: "ST"},
		Body:       []byte(`{"model":"glm-5.3-flash"}`),
		Benefit:    true,
	})
	if errChat != nil {
		t.Fatalf("chat: %v", errChat)
	}
	if *captured == nil {
		t.Fatal("没有截到请求")
	}
	if got := (*captured).Header.Get(HeaderMaasType); got != MaasBenefit {
		t.Fatalf("%s = %q, want %q", HeaderMaasType, got, MaasBenefit)
	}
	if signed := (*captured).Header.Get("Authorization"); !strings.Contains(signed, "maas_type") {
		t.Fatalf("Authorization 未覆盖 %s: %s", HeaderMaasType, signed)
	}
}

// TestChatRequestWithoutBenefitChannel 守住反向：显式走积分通道时不带这个头。
func TestChatRequestWithoutBenefitChannel(t *testing.T) {
	captured := stubHostHTTPDo(t)

	_, errChat := Chat(context.Background(), "", ChatRequest{
		Credential: &Credential{AccessKeyID: "AK", SecretAccessKey: "SK"},
		Body:       []byte(`{"model":"glm-5.2"}`),
	})
	if errChat != nil {
		t.Fatalf("chat: %v", errChat)
	}
	if *captured != nil && (*captured).Header.Get(HeaderMaasType) != "" {
		t.Fatalf("积分通道不应带 %s，实际 %q", HeaderMaasType, (*captured).Header.Get(HeaderMaasType))
	}
}

// TestBuildAuthorizeURLCarriesCallbackPort 锁定授权链接的 port 参数。
//
// 门户按 port 拼回调地址；少了它就回落到 80 端口，浏览器停在打不开的
// 127.0.0.1 地址上，插件则永远等不到回调。
func TestBuildAuthorizeURLCarriesCallbackPort(t *testing.T) {
	url := buildAuthorizeURL("ticket-1", "challenge-1")
	for _, want := range []string{
		"port=" + strconv.Itoa(LocalCallbackPort),
		"client_id=" + ClientID,
		"ticket_id=ticket-1",
		"code_challenge_method=SHA-256",
		"plugin-name=" + PluginName,
	} {
		if !strings.Contains(url, want) {
			t.Fatalf("授权链接缺少 %s: %s", want, url)
		}
	}
	// 票据密钥不能进授权链接：它会出现在浏览器历史与日志里。
	if strings.Contains(url, "secret") {
		t.Fatalf("授权链接不应携带 secret: %s", url)
	}
}

// TestRequestTokenUsesSTSHost 锁定令牌端点主机。
//
// /v1/oauth2/tokens 只在 sts 上，snap-manager 上没有这个 API（实测 404 APIG.0101）。
func TestRequestTokenUsesSTSHost(t *testing.T) {
	if !strings.Contains(STSHost, "sts.cn-north-4.myhuaweicloud.com") {
		t.Fatalf("STSHost = %q", STSHost)
	}
	if strings.Contains(STSHost, "snap-manager") {
		t.Fatalf("令牌端点不应指向 snap-manager: %q", STSHost)
	}
}

// TestFetchQuotaTreatsMissingBenefitAsZeroNotError 锁定「账号没有福利权益」的呈现方式。
//
// 上游对这种账号回 4004 benefit not found（实测：带签名、带各种身份头都一样），
// 同一账号的福利模型清单正常、签到也返回成功——所以这是账号状态，不是查询失败。
// 当成错误会让控制台整列显示「-- / --」，用户看不出到底是没有额度还是查询坏了。
func TestFetchQuotaTreatsMissingBenefitAsZeroNotError(t *testing.T) {
	stubHostWithBody(t, `{"error_code":"4004","error_msg":"benefit not found","result":null}`)

	quota, errQuota := FetchQuota(context.Background(), &Credential{AccessKeyID: "AK", SecretAccessKey: "SK"})
	if errQuota != nil {
		t.Fatalf("没有福利权益不该报错: %v", errQuota)
	}
	if quota == nil || quota.Subscription == nil {
		t.Fatal("额度结果为空")
	}
	if len(quota.Summary) == 0 || quota.Summary[0].Key != "total" {
		t.Fatalf("未开通也要给出 total 键，否则控制台折算不出总额: %+v", quota.Summary)
	}
	if !strings.Contains(quota.Summary[0].Label, "未开通") {
		t.Fatalf("标签要说明原因: %q", quota.Summary[0].Label)
	}
}

// TestFetchQuotaUsesStandardRemainingKeys 锁定标准键。
//
// 控制台的「剩余 / 总额」只认 remaining / total / used（internal/auth.Totals），
// 用自定义键会让这两列永远是 0 / 0。
func TestFetchQuotaUsesStandardRemainingKeys(t *testing.T) {
	stubHostWithBody(t, `{"error_code":"0000","result":{"channel":"benefit",`+
		`"daily_token_limit":10000000,"daily_tokens_used":2500000,`+
		`"monthly_token_limit":100000000,"monthly_tokens_used":40000000}}`)

	quota, errQuota := FetchQuota(context.Background(), &Credential{AccessKeyID: "AK", SecretAccessKey: "SK"})
	if errQuota != nil {
		t.Fatalf("fetch quota: %v", errQuota)
	}
	values := map[string]float64{}
	for _, metric := range quota.Summary {
		values[metric.Key] = metric.Value
	}
	if values["total"] != 10000000 {
		t.Fatalf("total = %v, want 10000000", values["total"])
	}
	// 剩余必须由「每日额度 - 今日已用」算出来，而不是回传上游没有的字段。
	if values["remaining"] != 7500000 {
		t.Fatalf("remaining = %v, want 7500000", values["remaining"])
	}
	if values["used"] != 2500000 {
		t.Fatalf("used = %v, want 2500000", values["used"])
	}
}
