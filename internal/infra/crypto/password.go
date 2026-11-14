package crypto

import "github.com/hwhkit/hwhkit-go/secrets"

// HashPassword 转发 secrets.HashPassword（argon2id PHC，参数与存量一致）。
func HashPassword(password string) (string, error) { return secrets.HashPassword(password) }

// VerifyPassword 转发 secrets.VerifyPassword（恒定时间比较）。
func VerifyPassword(encoded, password string) bool { return secrets.VerifyPassword(encoded, password) }
