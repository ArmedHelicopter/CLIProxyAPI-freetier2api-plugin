package tests

// 本文件用静态 import 图守住分层边界。

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

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
