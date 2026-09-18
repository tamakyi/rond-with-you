package auth

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/subtle"
	"encoding/base32"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
	"time"
)

// TOTP 实现（RFC 6238，30 秒步长、6 位数字、HMAC-SHA1），
// 与 Google Authenticator / 1Password / Microsoft Authenticator 兼容。

const (
	totpStep   = 30 * time.Second
	totpStepS  = 30 // 步长（秒），计数器 = unix秒 / 30
	totpWindow = 1  // 允许前后偏差的步数
)

// counterOf 把时刻换算成 TOTP 计数器。
func counterOf(unixSec int64) uint64 {
	if unixSec < 0 {
		return 0
	}
	return uint64(unixSec / totpStepS)
}

var ErrTOTPNotEnabled = errors.New("未开启两步验证")

// GenerateTOTPSecret 生成 20 字节随机秘钥的 base32 编码（无填充）。
func GenerateTOTPSecret() (string, error) {
	raw := make([]byte, 20)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(raw), nil
}

func totpCode(secret string, counter uint64) (string, error) {
	key, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(
		strings.ToUpper(strings.ReplaceAll(secret, " ", "")))
	if err != nil {
		return "", fmt.Errorf("秘钥格式无效: %w", err)
	}
	var msg [8]byte
	binary.BigEndian.PutUint64(msg[:], counter)
	m := hmac.New(sha1.New, key)
	m.Write(msg[:])
	sum := m.Sum(nil)
	off := sum[len(sum)-1] & 0x0f
	code := (binary.BigEndian.Uint32(sum[off:off+4]) & 0x7fffffff) % 1_000_000
	return fmt.Sprintf("%06d", code), nil
}

// TOTPCode 返回指定时刻的验证码（用于展示与测试）。
func TOTPCode(secret string, t time.Time) (string, error) {
	return totpCode(secret, counterOf(t.Unix()))
}

// VerifyTOTP 校验验证码，允许前后各一个步长的时钟偏差。
func VerifyTOTP(secret, code string) bool {
	now := counterOf(time.Now().Unix())
	for _, c := range []uint64{now - totpWindow, now, now + totpWindow} {
		want, err := totpCode(secret, c)
		if err != nil {
			return false
		}
		if subtle.ConstantTimeCompare([]byte(want), []byte(strings.TrimSpace(code))) == 1 {
			return true
		}
	}
	return false
}

// OTPAuthURL 生成验证器 App 扫码用的 URI。
func OTPAuthURL(username, secret string) string {
	label := urlEscape("rond-with-you:" + username)
	return "otpauth://totp/" + label + "?secret=" + secret + "&issuer=" + urlEscape("rond-with-you") +
		"&algorithm=SHA1&digits=6&period=30"
}

func urlEscape(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_', r == '.', r == '~', r == ':':
			b.WriteRune(r)
		default:
			fmt.Fprintf(&b, "%%%02X", r)
		}
	}
	return b.String()
}

// ---------- 用户秘钥存取 ----------

// TOTPState 返回用户的两步验证状态。
func (s *Store) TOTPState(ctx context.Context, userID int64) (secret string, enabled bool, err error) {
	err = s.DB.QueryRowContext(ctx, `SELECT COALESCE(totp_secret,''), totp_enabled
		FROM users WHERE id=$1`, userID).Scan(&secret, &enabled)
	return
}

// SetTOTPSecret 保存（或覆盖）秘钥；enabled 保持不变，供「先展示二维码再确认」流程。
func (s *Store) SetTOTPSecret(ctx context.Context, userID int64, secret string) error {
	_, err := s.DB.ExecContext(ctx, `UPDATE users SET totp_secret=$1 WHERE id=$2`, secret, userID)
	return err
}

// EnableTOTP 确认启用两步验证。
func (s *Store) EnableTOTP(ctx context.Context, userID int64) error {
	_, err := s.DB.ExecContext(ctx, `UPDATE users SET totp_enabled=TRUE WHERE id=$1 AND totp_secret<>''`, userID)
	return err
}

// DisableTOTP 关闭两步验证并清除秘钥。
func (s *Store) DisableTOTP(ctx context.Context, userID int64) error {
	_, err := s.DB.ExecContext(ctx, `UPDATE users SET totp_enabled=FALSE, totp_secret='' WHERE id=$1`, userID)
	return err
}

// ---------- 两步登录的中间凭据 ----------

// SignPending2FA 在口令校验通过但未过两步验证时签发短时中间令牌，
// 绑定口令摘要（改密即失效），有效期 5 分钟。
func (s *Store) SignPending2FA(userID int64, hash string) string {
	exp := time.Now().Add(5 * time.Minute).Unix()
	payload := fmt.Sprintf("2fa|%d|%d", userID, exp)
	return payload + "|" + s.mac("p"+payload, hash)
}

// ParsePending2FA 还原中间令牌。
func (s *Store) ParsePending2FA(token, hash string) (int64, bool) {
	parts := strings.Split(token, "|")
	if len(parts) != 4 || parts[0] != "2fa" {
		return 0, false
	}
	var uid, exp int64
	if _, err := fmt.Sscanf(parts[1], "%d", &uid); err != nil {
		return 0, false
	}
	if _, err := fmt.Sscanf(parts[2], "%d", &exp); err != nil {
		return 0, false
	}
	if time.Now().Unix() > exp {
		return 0, false
	}
	payload := "2fa|" + parts[1] + "|" + parts[2]
	if !hmac.Equal([]byte(parts[3]), []byte(s.mac("p"+payload, hash))) {
		return 0, false
	}
	return uid, true
}
