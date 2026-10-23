package tests

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// 本文件用静态 import 图守住分层边界。

const vendorImportPrefix = "freetier2api-plugin/internal/vendors/"

func TestNoCrossVendorImports(t *testing.T) {
	root := filepath.Join("..", "internal", "vendors")
	errWalk := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		owner := vendorOwner(path)
		if owner == "" {
			return nil
		}
		for _, imp := range fileImports(t, path) {
			if !strings.HasPrefix(imp, vendorImportPrefix) {
				continue
			}
			target := strings.Split(strings.TrimPrefix(imp, vendorImportPrefix), "/")[0]
			if target != owner {
				t.Errorf("%s：供应商包 %q 不得 import 另一个供应商包 %q", path, owner, target)
			}
		}
		return nil
	})
	if errWalk != nil {
		t.Fatalf("walk vendors: %v", errWalk)
	}
}

func TestPluginAppVendorImportsOnlyInCompositionRoot(t *testing.T) {
	dir := filepath.Join("..", "internal", "app")
	entries, errRead := os.ReadDir(dir)
	if errRead != nil {
		t.Fatalf("read app: %v", errRead)
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		if entry.Name() == "vendors.go" {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		for _, imp := range fileImports(t, path) {
			if strings.HasPrefix(imp, vendorImportPrefix) {
				t.Errorf("%s：只有组合根 vendors.go 可以直接 import 具体供应商", path)
			}
		}
	}
}

func vendorOwner(path string) string {
	parts := strings.Split(filepath.ToSlash(path), "/internal/vendors/")
	if len(parts) != 2 {
		return ""
	}
	return strings.Split(parts[1], "/")[0]
}

func fileImports(t *testing.T, path string) []string {
	t.Helper()
	file, errParse := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
	if errParse != nil {
		t.Fatalf("parse %s: %v", path, errParse)
	}
	out := make([]string, 0, len(file.Imports))
	for _, spec := range file.Imports {
		value, errUnquote := strconv.Unquote(spec.Path.Value)
		if errUnquote != nil {
			t.Fatalf("unquote import in %s: %v", path, errUnquote)
		}
		out = append(out, value)
	}
	return out
}

func TestTaskEngineVendorNeutral(t *testing.T) {
	dir := filepath.Join("..", "internal", "tasks")
	entries, errRead := os.ReadDir(dir)
	if errRead != nil {
		t.Fatalf("read tasks: %v", errRead)
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		for _, imp := range fileImports(t, path) {
			if strings.HasPrefix(imp, vendorImportPrefix) || imp == "freetier2api-plugin/internal/admin" {
				t.Errorf("%s：任务引擎不得依赖供应商或管理接口（%s）", path, imp)
			}
		}
	}

	coreTask := filepath.Join("..", "internal", "core", "task.go")
	raw, errRead := os.ReadFile(coreTask)
	if errRead != nil {
		t.Fatalf("read core/task.go: %v", errRead)
	}
	lower := strings.ToLower(string(raw))
	for _, banned := range []string{"school", "growth"} {
		if strings.Contains(lower, banned) {
			t.Errorf("internal/core/task.go：通用任务契约不得出现供应商专属标识 %q", banned)
		}
	}
}

func TestCoreDoesNotImportUpperLayers(t *testing.T) {
	entries, errRead := os.ReadDir(filepath.Join("..", "internal", "core"))
	if errRead != nil {
		t.Fatalf("read core: %v", errRead)
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		path := filepath.Join("..", "internal", "core", entry.Name())
		for _, imp := range fileImports(t, path) {
			if strings.HasPrefix(imp, "freetier2api-plugin/internal/") {
				t.Errorf("%s：internal/core 只能依赖 cpasdk 与标准库（发现 %s）", path, imp)
			}
		}
	}
}

func TestTestLayout(t *testing.T) {
	rootEntries, errRead := os.ReadDir(".")
	if errRead != nil {
		t.Fatalf("read tests root: %v", errRead)
	}
	for _, entry := range rootEntries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		if entry.Name() != "architecture_test.go" {
			t.Errorf("tests/%s：集成测试应放在 tests/integration/，tests/ 根只保留架构守卫", entry.Name())
		}
	}

	integrationEntries, errRead := os.ReadDir(filepath.Join("integration"))
	if errRead != nil {
		t.Fatalf("read tests/integration: %v", errRead)
	}
	for _, entry := range integrationEntries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".go") && !strings.HasSuffix(entry.Name(), "_test.go") {
			t.Errorf("tests/integration/%s：集成测试目录不应包含非测试源码", entry.Name())
		}
	}
}

// layerRule 描述一个内部包的依赖边界。
//
// 之所以用一张显式的表而不是逐个 if：重构时新增/删除内部包必须同步这张表，
// 「忘记更新分层规则」会立刻在 CI 里失败，而不是靠人记住。
type layerRule struct {
	pkg string
	// forbidden 是该包**不允许**import 的 internal 包（精确到一级目录）。
	forbidden []string
	// allowlist 非空时，import 集合必须完全落在其中（供应商包用）。
	allowlist []string
}

// layerRules 是仓库的分层契约。
//
// 依赖方向自上而下：app -> admin / tasks / auth / hostbridge / vendors -> core。
// 同层之间不互相依赖（admin 与 tasks、tasks 与 auth 都禁止）。
//
// 供应商包用 allowlist 而不是 forbidden：它们的依赖面必须**恰好**是
// core（契约）+ hostbridge（宿主通信）+ logger（日志）三者，多一个都不行——
// 这条规则挡住了 vendor 反向依赖 auth / state / tasks / admin 的所有可能。
var layerRules = []layerRule{
	{pkg: "core", forbidden: []string{"admin", "app", "auth", "hostbridge", "logger", "state", "tasks", "vendors"}},
	{pkg: "logger"},
	{pkg: "state", forbidden: []string{"admin", "app", "auth", "hostbridge", "tasks", "vendors"}},
	{pkg: "hostbridge", forbidden: []string{"admin", "app", "auth", "state", "tasks", "vendors"}},
	{pkg: "auth", forbidden: []string{"admin", "app", "tasks", "vendors"}},
	{pkg: "tasks", forbidden: []string{"admin", "app", "vendors"}},
	{pkg: "admin", forbidden: []string{"app"}},
	{pkg: "vendors/cline", allowlist: []string{"core", "hostbridge", "logger", "vendors/cline"}},
	{pkg: "vendors/codearts", allowlist: []string{"core", "hostbridge", "logger", "vendors/codearts"}},
	{pkg: "vendors/minimaxcode", allowlist: []string{"core", "hostbridge", "logger", "vendors/minimaxcode"}},
	{pkg: "vendors/opencodezen", allowlist: []string{"core", "hostbridge", "logger", "vendors/opencodezen"}},
	{pkg: "vendors/qoder", allowlist: []string{"core", "hostbridge", "logger", "vendors/qoder"}},
	{pkg: "vendors/trae", allowlist: []string{"core", "hostbridge", "logger", "vendors/trae"}},
	{pkg: "vendors/workbuddy", allowlist: []string{"core", "hostbridge", "logger", "vendors/workbuddy"}},
	{pkg: "vendors/zcode", allowlist: []string{"core", "hostbridge", "logger", "vendors/zcode"}},
}

// internalPackages 是 internal/ 下一级允许存在的目录名白名单。
//
// 它的作用是**发现问题**：新增一个包却忘了在 layerRules 里定边界时，
// 白名单会先把它拦下来，提示补规则，而不是让它静默进入依赖图。
var internalPackages = map[string]bool{
	"admin": true, "app": true, "auth": true, "core": true,
	"hostbridge": true, "logger": true, "state": true, "tasks": true, "vendors": true,
}

func TestInternalPackageAllowlist(t *testing.T) {
	entries, errRead := os.ReadDir(filepath.Join("..", "internal"))
	if errRead != nil {
		t.Fatalf("read internal: %v", errRead)
	}
	for _, entry := range entries {
		if !entry.IsDir() || !internalPackages[entry.Name()] {
			t.Errorf("internal/%s：新增内部包必须同步 internalPackages 与 layerRules（分层守卫要求显式声明边界）", entry.Name())
		}
	}
}

// TestDeclaredPackagesHaveBoundaryRules prevents a package allowlist entry from
// silently bypassing the dependency boundary checks. app is the composition root
// and is intentionally unrestricted; vendors is covered by its per-vendor rules.
func TestDeclaredPackagesHaveBoundaryRules(t *testing.T) {
	covered := make(map[string]bool, len(layerRules))
	for _, rule := range layerRules {
		covered[strings.Split(rule.pkg, "/")[0]] = true
	}
	for pkg := range internalPackages {
		if pkg == "app" || pkg == "vendors" {
			continue
		}
		if !covered[pkg] {
			t.Errorf("internal/%s is allowlisted but has no layerRules boundary", pkg)
		}
	}
}

func TestLayerBoundaries(t *testing.T) {
	root := filepath.Join("..", "internal")
	for _, rule := range layerRules {
		dir := filepath.Join(root, filepath.FromSlash(rule.pkg))
		entries, errRead := os.ReadDir(dir)
		if errRead != nil {
			t.Fatalf("read %s: %v", rule.pkg, errRead)
		}
		banned := make(map[string]bool, len(rule.forbidden))
		for _, name := range rule.forbidden {
			banned[name] = true
		}
		allowed := make(map[string]bool, len(rule.allowlist))
		for _, name := range rule.allowlist {
			allowed[name] = true
		}
		for _, entry := range entries {
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
				continue
			}
			path := filepath.Join(dir, entry.Name())
			for _, imp := range fileImports(t, path) {
				if !strings.HasPrefix(imp, "freetier2api-plugin/internal/") {
					continue
				}
				target := strings.TrimPrefix(imp, "freetier2api-plugin/internal/")
				if banned[strings.Split(target, "/")[0]] {
					t.Errorf("%s：internal/%s 不得依赖 internal/%s", path, rule.pkg, target)
				}
				if len(allowed) > 0 && !allowed[target] {
					t.Errorf("%s：internal/%s 只能依赖 %v，实际依赖了 %s",
						path, rule.pkg, rule.allowlist, target)
				}
			}
		}
	}
}

// TestVendorsNeverWriteCredentialsDirectly 守住「凭证写回只有一条路径」。
//
// 分层 allowlist 只限制 vendor 只能依赖 core/hostbridge/logger，但 hostbridge
// 同时提供 HTTP 桥与宿主回调，光靠 import 图看不出「这个 vendor 在发
// host.auth.save」。凭证写回（合并 → host.auth.save → 落盘）必须由
// internal/tasks 的保活编排统一做，供应商只实现 core.Refresher 刷新 token——
// 否则每加一个供应商就多一份会漂移的写回实现。
func TestVendorsNeverWriteCredentialsDirectly(t *testing.T) {
	forbidden := []string{"MethodHostAuthSave", "MethodAuthSave", "host.auth.save"}
	root := filepath.Join("..", "internal", "vendors")
	errWalk := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		raw, errRead := os.ReadFile(path)
		if errRead != nil {
			return errRead
		}
		body := stripComments(string(raw))
		for _, token := range forbidden {
			if strings.Contains(body, token) {
				t.Errorf("%s：供应商不得自行写回凭证（出现 %q）——刷新 token 只实现 core.Refresher，写回由 internal/tasks 统一编排", path, token)
			}
		}
		return nil
	})
	if errWalk != nil {
		t.Fatalf("walk vendors: %v", errWalk)
	}
}

// stripComments 去掉 Go 源码里的注释，只留可编译的代码文本。
//
// 守卫要在代码里找符号，而不是在解释这个符号的注释里找——否则开发者
// 一旦在注释里说明「这里不该出现 host.auth.save」就会把自己绊在守卫上。
func stripComments(src string) string {
	var out strings.Builder
	for i := 0; i < len(src); {
		switch {
		case strings.HasPrefix(src[i:], "//"):
			end := strings.IndexByte(src[i:], '\n')
			if end < 0 {
				i = len(src)
			} else {
				i += end
			}
		case strings.HasPrefix(src[i:], "/*"):
			end := strings.Index(src[i+2:], "*/")
			if end < 0 {
				i = len(src)
			} else {
				i += end + 4
			}
			out.WriteByte(' ')
		case src[i] == '\'' || src[i] == '"' || src[i] == '`':
			quote := src[i]
			out.WriteByte(quote)
			i++
			for i < len(src) {
				if quote != '`' && src[i] == '\\' {
					out.WriteByte(src[i])
					i++
					if i < len(src) {
						out.WriteByte(src[i])
						i++
					}
					continue
				}
				out.WriteByte(src[i])
				if src[i] == quote {
					i++
					break
				}
				i++
			}
		default:
			out.WriteByte(src[i])
			i++
		}
	}
	return out.String()
}
