package securevalue

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
)

// Seal 加密并封装byte列表。
// Seal encrypts a small sensitive value with AES-GCM. Production deployments
// provide the key through secret management; the database never stores it.
func Seal(key string, plaintext string) ([]byte, error) {
	block, err := aes.NewCipher(deriveKey(key))
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}
	return []byte(base64.RawStdEncoding.EncodeToString(gcm.Seal(nonce, nonce, []byte(plaintext), nil))), nil
}

// Open 解密并返回字符串。
func Open(key string, ciphertext []byte) (string, error) {
	raw, err := base64.RawStdEncoding.DecodeString(string(ciphertext))
	if err != nil {
		return "", fmt.Errorf("decode sealed value: %w", err)
	}
	block, err := aes.NewCipher(deriveKey(key))
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	if len(raw) < gcm.NonceSize() {
		return "", fmt.Errorf("sealed value is truncated")
	}
	plaintext, err := gcm.Open(nil, raw[:gcm.NonceSize()], raw[gcm.NonceSize():], nil)
	if err != nil {
		return "", fmt.Errorf("open sealed value: %w", err)
	}
	return string(plaintext), nil
}

// HMAC 使用指定密钥计算并返回消息认证码。
func HMAC(key string, parts ...string) string {
	mac := hmac.New(sha256.New, []byte(key))
	for _, part := range parts {
		_, _ = mac.Write([]byte{0})
		_, _ = mac.Write([]byte(part))
	}
	return hex.EncodeToString(mac.Sum(nil))
}

// EqualHMAC 判断Equal HMAC。
func EqualHMAC(expected string, actual string) bool {
	return subtle.ConstantTimeCompare([]byte(expected), []byte(actual)) == 1
}

// Digest 计算字符串的摘要。
func Digest(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

// deriveKey 根据输入字符串派生固定长度的加密密钥。
func deriveKey(value string) []byte {
	sum := sha256.Sum256([]byte(value))
	return sum[:]
}
