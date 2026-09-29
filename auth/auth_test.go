package auth

import (
	"testing"
	"time"
)

// newTestAuth 创建一个指向不可达 AD 的认证器，用于测试不会触达 AD 的本地校验分支。
func newTestAuth(t *testing.T) *Authenticator {
	t.Helper()
	cfg := &Config{
		AD: ADConfig{
			Server:          "127.0.0.1:1", // 不可达：本地校验分支不应触达它
			ManagerDN:       "test",
			ManagerPassword: "test",
			SearchDN:        "test",
			TLSInsecure:     true,
		},
		RequirePassword: false, // 目录查询模式，方便不传密码回归
		ADTimeout:       time.Second,
		MgmtTTLd:        8 * time.Hour,
	}
	return NewAuthenticator(cfg)
}

func TestLoginRejectsInvalidFormatWithoutAD(t *testing.T) {
	a := newTestAuth(t)
	cases := []string{"", "a b", "#0", "Admin", "UPPER", "-bad", "0start", "with space", "user name"}
	for _, c := range cases {
		if _, _, _, err := a.Login(c, ""); err != ErrInvalidFormat {
			t.Errorf("Login(%q) = %v, want ErrInvalidFormat", c, err)
		}
	}
}

func TestLoginRejectsRoot(t *testing.T) {
	a := newTestAuth(t)
	if _, _, _, err := a.Login("root", ""); err != ErrRootNotAllowed {
		t.Errorf("Login(root) = %v, want ErrRootNotAllowed", err)
	}
}

func TestLoginValidFormatReachesAD(t *testing.T) {
	// 合法格式用户名会触达 AD（此处 AD 不可达 → 应返回 ErrServiceUnavailable，即 fail-closed）。
	a := newTestAuth(t)
	if _, _, _, err := a.Login("nobody", ""); err != ErrServiceUnavailable {
		t.Errorf("Login(nobody) = %v, want ErrServiceUnavailable (fail-closed)", err)
	}
}

// TestLoginRequirePasswordMissingPassword 验证 requirePassword 开启时空密码直接返回 ErrInvalidFormat（400），不触达 AD。
func TestLoginRequirePasswordMissingPassword(t *testing.T) {
	cfg := &Config{
		AD: ADConfig{
			Server:          "127.0.0.1:1",
			ManagerDN:       "test",
			ManagerPassword: "test",
			SearchDN:        "test",
			TLSInsecure:     true,
		},
		RequirePassword: true,
		ADTimeout:       time.Second,
		MgmtTTLd:        8 * time.Hour,
	}
	a := NewAuthenticator(cfg)

	// 空密码：即使用户名合法也直接 400，不触达 AD。
	if _, _, _, err := a.Login("nobody", ""); err != ErrInvalidFormat {
		t.Errorf("Login(nobody, \"\") = %v, want ErrInvalidFormat (missing password)", err)
	}

	// 非法用户名（不触发空密码逻辑，仍按格式非法）。
	if _, _, _, err := a.Login("a b", ""); err != ErrInvalidFormat {
		t.Errorf("Login(\"a b\", \"\") = %v, want ErrInvalidFormat", err)
	}
}

func TestEmailRegexp(t *testing.T) {
	valid := []string{"a@b.com", "user.name@example.com", "x.y_z@domain.co.uk"}
	invalid := []string{"", "a@b", "a@", "@x.com", "a b@x.com", "a@b c.com", "no@at", "@", "a@b@c.com"}
	for _, v := range valid {
		if !emailRegexp.MatchString(v) {
			t.Errorf("expected %q to be valid", v)
		}
	}
	for _, v := range invalid {
		if emailRegexp.MatchString(v) {
			t.Errorf("expected %q to be invalid", v)
		}
	}
}

// TestLoginInvalidEmailDoesNotReachAD 验证非法邮箱格式直接返回 ErrInvalidEmail（400），不触达 AD。
func TestLoginInvalidEmailDoesNotReachAD(t *testing.T) {
	a := newTestAuth(t)
	cases := []string{"a@b", "a@", "@x.com", "a b@x.com", "a@b c.com"}
	for _, c := range cases {
		if _, _, _, err := a.Login(c, ""); err != ErrInvalidEmail {
			t.Errorf("Login(%q) = %v, want ErrInvalidEmail", c, err)
		}
	}
}

// TestLoginValidEmailReachesAD 验证合法格式邮箱会触达 AD（此处 AD 不可达 → ErrServiceUnavailable，fail-closed）。
func TestLoginValidEmailReachesAD(t *testing.T) {
	a := newTestAuth(t)
	if _, _, _, err := a.Login("nobody@example.com", ""); err != ErrServiceUnavailable {
		t.Errorf("Login(nobody@example.com) = %v, want ErrServiceUnavailable (fail-closed)", err)
	}
}

// TestLoginEmailRoot 验证邮箱解析出的账号名若为 root 应拒绝（此处解析走 AD，不可达时失败，不适用 root 判定，
// 因此仅验证合法邮箱在 requirePassword=false 下触达 AD 的 fail-closed 行为）。
func TestLoginEmailContainsAtDetection(t *testing.T) {
	// 含 @ 的输入判定为邮箱（走 emailRegexp），非法格式返回 ErrInvalidEmail。
	a := newTestAuth(t)
	if _, _, _, err := a.Login("a@b", ""); err != ErrInvalidEmail {
		t.Errorf("Login(a@b) = %v, want ErrInvalidEmail", err)
	}
	// 不含 @ 但格式非法的账号名返回 ErrInvalidFormat。
	if _, _, _, err := a.Login("bad name", ""); err != ErrInvalidFormat {
		t.Errorf("Login(bad name) = %v, want ErrInvalidFormat", err)
	}
}

func TestUsernameRegexp(t *testing.T) {
	valid := []string{"a", "alice", "alice_1", "alice-bob", "_underscore", "a1234567890123456789012345678901"}
	invalid := []string{"", "A", "1abc", "-a", "a b", "a#b", "alice@corp", "x" + pad(32)}
	for _, v := range valid {
		if !usernameRegexp.MatchString(v) {
			t.Errorf("expected %q to be valid", v)
		}
	}
	for _, v := range invalid {
		if usernameRegexp.MatchString(v) {
			t.Errorf("expected %q to be invalid", v)
		}
	}
}

func pad(n int) string {
	s := ""
	for i := 0; i < n; i++ {
		s += "x"
	}
	return s
}

// issueFor 绕过 AD，直接向认证器内置 mgmt 存储签发 token（用于测试校验/吊销逻辑）。
func (a *Authenticator) issueFor(t *testing.T, username string) string {
	t.Helper()
	tok, err := a.mgmt.Issue(username)
	if err != nil {
		t.Fatalf("Issue(%q): %v", username, err)
	}
	return tok
}

func TestAuthenticatorMgmtWrappers(t *testing.T) {
	a := newTestAuth(t)

	// 无效 mgmt token。
	if _, ok := a.ValidateMgmt("bogus"); ok {
		t.Error("ValidateMgmt(bogus) should fail")
	}

	tok := a.issueFor(t, "nobody")
	user, ok := a.ValidateMgmt(tok)
	if !ok || user != "nobody" {
		t.Fatalf("ValidateMgmt valid: ok=%v user=%q", ok, user)
	}
	// 幂等：可重复校验。
	if user, ok := a.ValidateMgmt(tok); !ok || user != "nobody" {
		t.Fatalf("ValidateMgmt repeat: ok=%v user=%q", ok, user)
	}

	// 吊销后失效。
	a.RevokeMgmt(tok)
	if _, ok := a.ValidateMgmt(tok); ok {
		t.Error("ValidateMgmt after revoke should fail")
	}
}
