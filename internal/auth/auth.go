package auth

import (
	"context"
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

const (
	iterations = 210_000
	keyLen     = 32
	saltLen    = 16
)

var ErrInvalidCredentials = errors.New("用户名或密码错误")

// HashPassword 使用 PBKDF2-SHA256 生成口令摘要。
func HashPassword(password string) (string, error) {
	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	dk, err := pbkdf2.Key(sha256.New, password, salt, iterations, keyLen)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("pbkdf2$sha256$%d$%s$%s", iterations, b64(salt), b64(dk)), nil
}

func VerifyPassword(encoded, password string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 5 || parts[0] != "pbkdf2" {
		return false
	}
	iter, err := strconv.Atoi(parts[2])
	if err != nil {
		return false
	}
	salt, err := unb64(parts[3])
	if err != nil {
		return false
	}
	want, err := unb64(parts[4])
	if err != nil {
		return false
	}
	got, err := pbkdf2.Key(sha256.New, password, salt, iter, len(want))
	if err != nil {
		return false
	}
	return subtle.ConstantTimeCompare(got, want) == 1
}

func b64(b []byte) string            { return base64.RawStdEncoding.EncodeToString(b) }
func unb64(s string) ([]byte, error) { return base64.RawStdEncoding.DecodeString(s) }

type User struct {
	ID          int64
	Username    string
	DisplayName string
	Hash        string
}

type Store struct {
	DB     *sql.DB
	Secret []byte
	Days   int
}

// Authenticate 校验用户口令。
func (s *Store) Authenticate(ctx context.Context, username, password string) (*User, error) {
	var u User
	err := s.DB.QueryRowContext(ctx, `SELECT id, username, COALESCE(display_name,''), password_hash
		FROM users WHERE username=$1`, username).Scan(&u.ID, &u.Username, &u.DisplayName, &u.Hash)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrInvalidCredentials
	}
	if err != nil {
		return nil, err
	}
	if !VerifyPassword(u.Hash, password) {
		return nil, ErrInvalidCredentials
	}
	if _, err := s.DB.ExecContext(ctx, `UPDATE users SET last_login_at=now() WHERE id=$1`, u.ID); err != nil {
		return nil, err
	}
	return &u, nil
}

// EnsureDefaultUser 在没有任何用户时创建初始管理员。
func (s *Store) EnsureDefaultUser(ctx context.Context, username, password string) (bool, error) {
	var n int
	if err := s.DB.QueryRowContext(ctx, `SELECT count(*) FROM users`).Scan(&n); err != nil {
		return false, err
	}
	if n > 0 {
		return false, nil
	}
	hash, err := HashPassword(password)
	if err != nil {
		return false, err
	}
	if _, err := s.DB.ExecContext(ctx, `INSERT INTO users (username, password_hash, display_name)
		VALUES ($1,$2,$3)`, username, hash, "站长"); err != nil {
		return false, err
	}
	return true, nil
}

func (s *Store) ChangePassword(ctx context.Context, userID int64, current, next string) error {
	var hash string
	if err := s.DB.QueryRowContext(ctx, `SELECT password_hash FROM users WHERE id=$1`, userID).Scan(&hash); err != nil {
		return err
	}
	if !VerifyPassword(hash, current) {
		return ErrInvalidCredentials
	}
	nh, err := HashPassword(next)
	if err != nil {
		return err
	}
	_, err = s.DB.ExecContext(ctx, `UPDATE users SET password_hash=$1 WHERE id=$2`, nh, userID)
	return err
}

// ErrUsernameTaken 表示新用户名已被占用。
var ErrUsernameTaken = errors.New("用户名已被占用")

// ChangeUsername 校验口令后改名。
// 会话 cookie 认的是 id 与口令摘要（都不含用户名），所以改名不会把当前登录踢掉。
func (s *Store) ChangeUsername(ctx context.Context, userID int64, current, name string) error {
	var hash string
	if err := s.DB.QueryRowContext(ctx, `SELECT password_hash FROM users WHERE id=$1`, userID).Scan(&hash); err != nil {
		return err
	}
	if !VerifyPassword(hash, current) {
		return ErrInvalidCredentials
	}
	var taken bool
	if err := s.DB.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM users WHERE username=$1 AND id<>$2)`, name, userID).Scan(&taken); err != nil {
		return err
	}
	if taken {
		return ErrUsernameTaken
	}
	_, err := s.DB.ExecContext(ctx, `UPDATE users SET username=$1 WHERE id=$2`, name, userID)
	return err
}

// UserByID 读取用户；用于每次请求还原登录态。
func (s *Store) UserByID(ctx context.Context, id int64) (*User, error) {
	var u User
	err := s.DB.QueryRowContext(ctx, `SELECT id, username, COALESCE(display_name,''), password_hash
		FROM users WHERE id=$1`, id).Scan(&u.ID, &u.Username, &u.DisplayName, &u.Hash)
	if err != nil {
		return nil, err
	}
	return &u, nil
}

// 会话令牌 = expiry|uid|hmac(uid|expiry|口令摘要前缀)，口令变更即自动失效。
func (s *Store) SignToken(userID int64, hash string) string {
	exp := time.Now().AddDate(0, 0, s.Days).Unix()
	payload := fmt.Sprintf("%d|%d", userID, exp)
	return payload + "|" + s.mac(payload, hash)
}

func (s *Store) ParseToken(token, hash string) (int64, bool) {
	parts := strings.Split(token, "|")
	if len(parts) != 3 {
		return 0, false
	}
	uid, err1 := strconv.ParseInt(parts[0], 10, 64)
	exp, err2 := strconv.ParseInt(parts[1], 10, 64)
	if err1 != nil || err2 != nil || time.Now().Unix() > exp {
		return 0, false
	}
	if !hmac.Equal([]byte(parts[2]), []byte(s.mac(parts[0]+"|"+parts[1], hash))) {
		return 0, false
	}
	return uid, true
}

func (s *Store) mac(payload, hash string) string {
	m := hmac.New(sha256.New, s.Secret)
	m.Write([]byte(payload))
	// 混入口令摘要，改密后旧 cookie 立即作废
	m.Write([]byte("|" + hash[:min(len(hash), 16)]))
	return b64(m.Sum(nil))
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
