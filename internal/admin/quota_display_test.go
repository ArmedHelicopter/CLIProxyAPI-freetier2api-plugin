package admin

import (
	"errors"
	"fmt"
	"net/http"
	"testing"

	"freetier2api-plugin/internal/core"
)

// TestIsAuthExpiredOnlyMatches401 锁定「只在 401 时才值得刷新一次」。
//
// 额度查询里任何 4xx/5xx 都触发一次保活是最省事的做法，但代价是上游改版或账号
// 无权限时也会白白多打一次上游，还可能把真正的错误盖住。只认 401。
func TestIsAuthExpiredOnlyMatches401(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"401 插件错误", core.NewPluginError("upstream_credential", "invalid access token", http.StatusUnauthorized), true},
		{"401 上游分类错误", &core.Failure{Kind: core.FailureKindCredential, Status: 401, Msg: "invalid access token"}, true},
		{"403 权限问题", core.NewPluginError("upstream_credential", "forbidden", http.StatusForbidden), false},
		{"502 上游异常", core.NewPluginError("upstream_request_failed", "bad gateway", http.StatusBadGateway), false},
		{"非凭证的分类错误", &core.Failure{Kind: core.FailureKindServer, Status: 500}, false},
		{"普通错误", errors.New("boom"), false},
		{"被包装的 401", fmt.Errorf("查询失败: %w", core.NewPluginError("upstream_credential", "expired", http.StatusUnauthorized)), true},
	}
	for _, c := range cases {
		if got := isAuthExpired(c.err); got != c.want {
			t.Fatalf("%s: isAuthExpired = %v, want %v", c.name, got, c.want)
		}
	}
}
