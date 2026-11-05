package execengine

// 引擎测试夹具（自 ir/ 测试迁移适配：平台配置构造改走 config 包导出 API）。

import (
	"strings"
	"testing"
	"time"

	"ir-agent-server/internal/infra/config"
)

// testPlatformConfig 返回一份通过校验的平台配置：走 PlatformConfigFromFile
// 拿到与生产一致的默认值（含 [gateway]/[local_agents]），只覆盖测试必需字段。
func testPlatformConfig(t *testing.T) *PlatformConfig {
	t.Helper()
	username := "admin"
	password := "correct horse battery staple"
	secret := strings.Repeat("s", 32)
	ttl := uint64(3600)
	baseURL := "http://127.0.0.1:8080"
	concurrency := 4
	allowLegacy := true
	root := &config.PlatformRootFile{
		Platform: config.PlatformSectionFile{
			PublicBaseURL:            &baseURL,
			GlobalMaxConcurrency:     &concurrency,
			AllowLegacyBodySignature: &allowLegacy,
		},
		Admin: config.AdminSectionFile{
			Username:       &username,
			Password:       &password,
			SessionSecret:  &secret,
			SessionTTLSecs: &ttl,
		},
	}
	cfg, err := config.PlatformConfigFromFile(root)
	if err != nil {
		t.Fatalf("build test platform config: %v", err)
	}
	return cfg
}

// parseTimestamp 解析存储的 RFC3339 时间戳（测试断言用，与存储层同口径）。
func parseTimestamp(value string) (time.Time, error) {
	return time.Parse(time.RFC3339, value)
}

func uint64Ptr(value uint64) *uint64 { return &value }
