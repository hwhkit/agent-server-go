// Package archguard 机械强制 doc/ARCHITECTURE-V2.md §1 的分层依赖规则。
//
// 规则（仅对生产文件 *_非_test.go 强制；集成测试允许跨层搭建夹具）：
//
//   - domain 只 import 标准库与 domain 内其他包（保持无环）；
//   - app    只 import 标准库与 domain；
//   - infra  import 标准库、domain、app（实现 app 定义的端口，依赖指向内层）
//     与第三方库（不得 import api）；
//   - api    只 import 标准库、domain、app 与第三方库（不得 import infra）；
//   - internal/** 生产代码一律不得 import 遗留包 ir-agent-server/ir
//     （绞杀者迁移期间旧包只被根 main.go 引用，直至 P6 切换后删除）。
package archguard

import (
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
)

const module = "ir-agent-server"

func layerOf(importPath string) string {
	rest := strings.TrimPrefix(importPath, module+"/internal/")
	if rest == importPath {
		return ""
	}
	switch {
	case strings.HasPrefix(rest, "domain/"):
		return "domain"
	case strings.HasPrefix(rest, "app/"):
		return "app"
	case strings.HasPrefix(rest, "infra/"):
		return "infra"
	case strings.HasPrefix(rest, "api/"):
		return "api"
	}
	return "other"
}

func isStdlib(importPath string) bool {
	first := importPath
	if i := strings.IndexByte(importPath, '/'); i >= 0 {
		first = importPath[:i]
	}
	return !strings.Contains(first, ".")
}

func TestLayerDependencies(t *testing.T) {
	fset := token.NewFileSet()
	root := filepath.Join("..", "..", "internal")
	violations := 0
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == "archguard" {
				return fs.SkipDir
			}
			return nil
		}
		name := d.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			return nil
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		layer := layerOf(module + "/internal/" + filepath.ToSlash(filepath.Dir(rel)))
		if layer == "" || layer == "other" {
			return nil
		}
		file, parseErr := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if parseErr != nil {
			t.Errorf("%s: parse: %v", rel, parseErr)
			violations++
			return nil
		}
		for _, imp := range file.Imports {
			importPath := strings.Trim(imp.Path.Value, `"`)
			if importPath == module+"/ir" || strings.HasPrefix(importPath, module+"/ir/") {
				t.Errorf("%s: layer %q must not import legacy package %s", rel, layer, importPath)
				violations++
				continue
			}
			target := layerOf(importPath)
			if target == "" {
				// 非 internal 包：第三方或标准库。
				if !isStdlib(importPath) && (layer == "domain" || layer == "app") {
					t.Errorf("%s: layer %q must not import third-party %s", rel, layer, importPath)
					violations++
				}
				continue
			}
			switch layer {
			case "domain":
				if target != "domain" {
					t.Errorf("%s: domain must not import %s (%s)", rel, importPath, target)
					violations++
				}
			case "app":
				if target != "domain" && target != "app" {
					t.Errorf("%s: app must not import %s (%s)", rel, importPath, target)
					violations++
				}
			case "infra":
				if target != "domain" && target != "infra" && target != "app" {
					t.Errorf("%s: infra must not import %s (%s)", rel, importPath, target)
					violations++
				}
			case "api":
				if target != "domain" && target != "app" && target != "api" {
					t.Errorf("%s: api must not import %s (%s)", rel, importPath, target)
					violations++
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk internal/: %v", err)
	}
	if violations == 0 {
		t.Logf("layer dependency rules hold across internal/")
	}
}

func TestDomainHasNoCycles(t *testing.T) {
	// domain 包间引用目前仅 agent → exec（Binding 值对象）。
	// 用简单的 DFS 检测包级引用环，防止后续演进引入循环依赖。
	fset := token.NewFileSet()
	root := filepath.Join("..", "..", "internal", "domain")
	deps := map[string][]string{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		if !strings.HasSuffix(d.Name(), ".go") || strings.HasSuffix(d.Name(), "_test.go") {
			return nil
		}
		pkgDir := filepath.ToSlash(filepath.Dir(path))
		file, parseErr := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if parseErr != nil {
			return parseErr
		}
		for _, imp := range file.Imports {
			importPath := strings.Trim(imp.Path.Value, `"`)
			if !strings.HasPrefix(importPath, module+"/internal/domain/") {
				continue
			}
			target := filepath.ToSlash(filepath.Join("internal/domain", strings.TrimPrefix(importPath, module+"/internal/domain/")))
			if target != pkgDir {
				deps[pkgDir] = append(deps[pkgDir], target)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk domain/: %v", err)
	}
	state := map[string]int{} // 0 未访问 1 在栈 2 完成
	var visit func(string) bool
	visit = func(pkg string) bool {
		switch state[pkg] {
		case 1:
			t.Errorf("import cycle detected at %s", pkg)
			return false
		case 2:
			return true
		}
		state[pkg] = 1
		ok := true
		for _, dep := range deps[pkg] {
			if !visit(dep) {
				ok = false
			}
		}
		state[pkg] = 2
		return ok
	}
	for pkg := range deps {
		visit(pkg)
	}
}
