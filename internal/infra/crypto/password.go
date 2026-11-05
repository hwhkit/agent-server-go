package crypto

import (
	"crypto/hmac"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

// argon2 口令哈希（PHC 字符串格式）。参数与存量系统/Rust argon2 crate
// 默认值逐项一致，保证既有 password_hash 在新实现下可校验。
const (
	argon2DefaultMemory  uint32 = 19456 // KiB
	argon2DefaultTime    uint32 = 2
	argon2DefaultThreads uint8  = 1
	argon2DefaultKeyLen  uint32 = 32
	argon2SaltLen               = 16
	argon2Version               = 19
)

// phcBase64 是 PHC 规定的 base64：标准字母表、不带填充。
var phcBase64 = base64.StdEncoding.WithPadding(base64.NoPadding)

// HashPassword 用 argon2id 默认参数生成 PHC 口令哈希：
// `$argon2id$v=19$m=19456,t=2,p=1$<salt>$<hash>`。
func HashPassword(password string) (string, error) {
	salt := make([]byte, argon2SaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("generate argon2 salt: %w", err)
	}
	digest := argon2.IDKey([]byte(password), salt,
		argon2DefaultTime, argon2DefaultMemory, argon2DefaultThreads, argon2DefaultKeyLen)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2Version, argon2DefaultMemory, argon2DefaultTime, argon2DefaultThreads,
		phcBase64.EncodeToString(salt), phcBase64.EncodeToString(digest)), nil
}

// VerifyPassword 校验 PHC 口令哈希。支持 argon2id 与 argon2i；使用哈希内
// 声明的参数重算（旧参数升级不需要重哈希）。恒定时间比较。
func VerifyPassword(encoded, password string) bool {
	if len(encoded) > 1024 {
		return false
	}
	parts := strings.Split(encoded, "$")
	// parts: ["", variant, v=…, m=…,t=…,p=…, salt, hash]
	if len(parts) != 6 || (parts[1] != "argon2id" && parts[1] != "argon2i") {
		return false
	}
	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil || version != argon2Version {
		return false
	}
	var memory, timeCost uint32
	var threads uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &memory, &timeCost, &threads); err != nil {
		return false
	}
	// 资源上限与旧栈一致：参数必须从存储哈希读出，但恶意/损坏的参数
	// 不能让校验变成 CPU/内存放大攻击。
	if memory == 0 || timeCost == 0 || threads == 0 {
		return false
	}
	if memory > 256*1024 || timeCost > 10 || threads > 16 {
		return false
	}
	salt, err := phcBase64.DecodeString(parts[4])
	if err != nil || len(salt) == 0 {
		return false
	}
	want, err := phcBase64.DecodeString(parts[5])
	if err != nil || len(want) == 0 {
		return false
	}
	var got []byte
	if parts[1] == "argon2id" {
		got = argon2.IDKey([]byte(password), salt, timeCost, memory, threads, uint32(len(want)))
	} else {
		got = argon2.Key([]byte(password), salt, timeCost, memory, threads, uint32(len(want)))
	}
	return hmac.Equal(got, want)
}

// ErrMalformedPHC 供调用方区分格式错误（可选）。
var ErrMalformedPHC = errors.New("malformed PHC hash")
