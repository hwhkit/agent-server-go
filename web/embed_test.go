package web_test

import (
	"strings"
	"testing"

	"ir-agent-server/web"
)

// 控制台资源完整性：四个文件都必须嵌入且非空；index.html 必须引用全部
// 三个资源（漏引用 = 部署后白屏，构建期不报错）。
func TestConsoleAssetsEmbedded(t *testing.T) {
	for _, name := range []string{"index.html", "app.css", "app.js", "i18n.js"} {
		data, err := web.Assets.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if len(data) == 0 {
			t.Errorf("%s is empty", name)
		}
	}
	index, err := web.Assets.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	for _, ref := range []string{"app.css", "app.js", "i18n.js"} {
		if !strings.Contains(string(index), ref) {
			t.Errorf("index.html does not reference %s", ref)
		}
	}
}

// i18n 完整性：en 字典不得缺 zh 已有键（缺失会回落中文，界面语言不一致）。
func TestI18NParity(t *testing.T) {
	script, err := web.Assets.ReadFile("i18n.js")
	if err != nil {
		t.Fatal(err)
	}
	text := string(script)
	zhStart := strings.Index(text, "zh: {")
	enStart := strings.Index(text, "en: {")
	if zhStart < 0 || enStart < 0 || enStart < zhStart {
		t.Fatal("i18n.js must define zh then en dictionaries")
	}
	extractKeys := func(section string) map[string]bool {
		keys := map[string]bool{}
		for _, line := range strings.Split(section, "\n") {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, `"`) {
				if key, _, found := strings.Cut(line, `":`); found {
					keys[strings.Trim(key, `"`)] = true
				}
			}
		}
		return keys
	}
	zhKeys := extractKeys(text[zhStart:enStart])
	enKeys := extractKeys(text[enStart:])
	if len(zhKeys) < 100 {
		t.Errorf("zh dictionary suspiciously small: %d keys", len(zhKeys))
	}
	missing := 0
	for key := range zhKeys {
		if !enKeys[key] {
			missing++
			if missing <= 5 {
				t.Errorf("en missing key %q", key)
			}
		}
	}
}
