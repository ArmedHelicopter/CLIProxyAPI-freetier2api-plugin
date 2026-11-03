package admin

// 控制台调用的每一个管理端点，都必须在 Registration().Routes 里声明过。
//
// 宿主只转发已注册的精确路由：漏注册的路径会被 CPA 直接 404（空响应体），
// 前端 resp.json() 于是抛 "Unexpected end of JSON input" —— 报错完全指不到
// 真正原因。0.1.35 的「一键任务」就踩了这个坑：Handle 里有分支、路由表没声明。

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

var consoleAPICall = regexp.MustCompile(`api\(\s*"([A-Za-z]+)"\s*,\s*"([^"$+]*)"`)

func TestConsoleCallsAreRegisteredRoutes(t *testing.T) {
	raw, err := os.ReadFile("console.html")
	if err != nil {
		t.Fatalf("read console.html: %v", err)
	}
	service := newService(t)
	id := service.deps.Identity()

	prefix := "/plugins/" + id.ID
	registered := map[string]bool{}
	for _, route := range service.Registration().Routes {
		registered[strings.ToUpper(route.Method)+" "+strings.TrimPrefix(route.Path, prefix)] = true
	}

	seen := map[string]bool{}
	for _, match := range consoleAPICall.FindAllStringSubmatch(string(raw), -1) {
		method, path := strings.ToUpper(match[1]), match[2]
		if index := strings.IndexByte(path, '?'); index >= 0 {
			path = path[:index]
		}
		if path == "" || strings.Contains(path, "%s") {
			continue
		}
		seen[method+" "+path] = true
		if !registered[method+" "+path] {
			t.Errorf("控制台调用 %s %s，但 Registration().Routes 未声明该路由", method, path)
		}
	}
	if len(seen) == 0 {
		t.Fatal("没有从 console.html 提取到任何 api() 调用，正则失效")
	}
	t.Logf("控制台调用的 %d 条路由全部已注册", len(seen))
}
