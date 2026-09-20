package store

import (
	"fmt"

	"golang.org/x/crypto/bcrypt"
)

// HashPassword 用 bcrypt 哈希管理员密码。
func HashPassword(pw string) (string, error) {
	b, err := bcrypt.GenerateFromPassword([]byte(pw), bcrypt.DefaultCost)
	if err != nil {
		return "", fmt.Errorf("哈希密码: %w", err)
	}
	return string(b), nil
}

// CheckPassword 常量时间比对密码哈希。
func CheckPassword(hash, pw string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(pw)) == nil
}
