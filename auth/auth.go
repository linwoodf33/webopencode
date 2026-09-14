package auth

import (
	"errors"
	"os/user"
	"regexp"
)

// 认证流程 sentinel error。
var (
	// ErrInvalidFormat 用户名缺失或格式非法（正则失败）。对应 400。
	ErrInvalidFormat = errors.New("invalid username format")
	// ErrInvalidEmail 邮箱格式非法。对应 400。
	ErrInvalidEmail = errors.New("invalid email format")
	// ErrRootNotAllowed root 不允许登录。对应 403。
	ErrRootNotAllowed = errors.New("root is not allowed")
	// ErrNotProvisioned 系统不存在该用户。对应 403。
	ErrNotProvisioned = errors.New("user not provisioned on this host")
	// ErrInvalidToken 一次性 token 无效/过期/已消费。用于 WS 升级前校验，对应 401。
	ErrInvalidToken = errors.New("invalid or expired token")
)

// usernameRegexp 校验用户名格式：小写字母/下划线开头，后续可含数字、下划线、连字符，最长 32。
var usernameRegexp = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)

// emailRegexp 校验邮箱格式：local-part@domain.tld（不允许空白、@ 等）。
var emailRegexp = regexp.MustCompile(`^[^@\s]+@[^@\s]+\.[^@\s]+$`)

// resolvedUser 保存从 AD 解析出的用户标识信息，供后续密码 bind、系统存在校验与 token 使用。
// sAMAccountName 为最终贯穿全程的账号名；userDN 为空表示未解析（纯账号名路径）。
type resolvedUser struct {
	sAMAccountName     string
	userDN             string
	userAccountControl int
}

// Authenticator 封装「AD 认证 + 正则 + 非root + 系统存在」的完整校验流程。
type Authenticator struct {
	ad              *ADClient
	token           *TokenStore
	requirePassword bool
}

// NewAuthenticator 基于配置创建一个认证器。
func NewAuthenticator(cfg *Config) *Authenticator {
	return &Authenticator{
		ad:              NewADClient(cfg),
		token:           NewTokenStore(cfg.TokenTTLd),
		requirePassword: cfg.RequirePassword,
	}
}

// ADClient 暴露 AD 客户端（供诊断使用）。
func (a *Authenticator) ADClient() *ADClient { return a.ad }

// RequirePassword 返回是否要求 AD 密码强认证。
func (a *Authenticator) RequirePassword() bool { return a.requirePassword }

// ResolveAccount 将原始登录标识（纯账号名或邮箱）解析为内部 resolvedUser。
//   - 含 @ → 视为邮箱：过 emailRegexp（失败→ErrInvalidEmail），调 AD 反查解析（无命中→ErrUserNotFound）。
//   - 不含 @ → 视为 sAMAccountName：过 usernameRegexp（失败→ErrInvalidFormat），
//     直接返回 {sAMAccountName: rawLogin}（userDN 为空）。
//
// 邮箱 local-part 不一定等于 sAMAccountName，因此必须经 AD 属性反查，绝不取 @ 前段。
func (a *Authenticator) ResolveAccount(rawLogin string) (*resolvedUser, error) {
	if rawLogin == "" {
		return nil, ErrInvalidFormat
	}

	hasAt := false
	for i := 0; i < len(rawLogin); i++ {
		if rawLogin[i] == '@' {
			hasAt = true
			break
		}
	}

	if hasAt {
		// 邮箱路径：本地格式校验先行，非法不触达 AD。
		if !emailRegexp.MatchString(rawLogin) {
			return nil, ErrInvalidEmail
		}
		return a.ad.ResolveEmail(rawLogin)
	}

	// 账号名路径：本地格式校验先行。
	if !usernameRegexp.MatchString(rawLogin) {
		return nil, ErrInvalidFormat
	}
	return &resolvedUser{sAMAccountName: rawLogin}, nil
}

// Login 执行完整认证流程，任一失败即拒绝：
//
//  1. 格式校验（账号名过 usernameRegexp，邮箱过 emailRegexp），非法输入不触达 AD；
//  2. 非 root 校验（对解析出的 sAMAccountName）；
//  3. 空密码校验（requirePassword 开启时密码缺失直接拒绝，不触达 AD）；
//  4. 解析标识：邮箱 → AD 反查取 sAMAccountName/DN/UAC；账号名 → 直接用该名；
//  5. AD 认证：requirePassword 开启 → 用户 DN+密码 bind 强认证；关闭 → 目录查询模式；
//  6. 系统存在校验（用解析出的 sAMAccountName），取 HomeDir 用于工作目录；
//  7. 签发一次性 token。
//
// 返回 token、规范化用户名（解析后的 sAMAccountName）、HomeDir。错误为 sentinel error。
func (a *Authenticator) Login(rawUsername, rawPassword string) (token, username, homeDir string, err error) {
	// 1. 空用户名直接拒绝。
	if rawUsername == "" {
		return "", "", "", ErrInvalidFormat
	}

	// 2. 判定标识类型并做本地格式校验（非法输入不触达 AD）。
	hasAt := false
	for i := 0; i < len(rawUsername); i++ {
		if rawUsername[i] == '@' {
			hasAt = true
			break
		}
	}
	if hasAt {
		if !emailRegexp.MatchString(rawUsername) {
			return "", "", "", ErrInvalidEmail
		}
	} else if !usernameRegexp.MatchString(rawUsername) {
		return "", "", "", ErrInvalidFormat
	}

	// 3. 空密码校验：requirePassword 开启时密码必须提供。
	if a.requirePassword && rawPassword == "" {
		return "", "", "", ErrInvalidFormat
	}

	// 4. 解析标识为内部 resolvedUser（邮箱走 AD 反查；账号名直接使用）。
	ru, rerr := a.resolveForLogin(rawUsername, hasAt)
	if rerr != nil {
		return "", "", "", rerr
	}
	username = ru.sAMAccountName

	// 5. 非 root 校验（对解析出的 sAMAccountName）。
	if username == "root" {
		return "", "", "", ErrRootNotAllowed
	}

	// 6. AD 认证（fail-closed：连接错误返回服务端故障，用户无效返回未授权）。
	var ok bool
	var aerr error
	if a.requirePassword {
		if ru.userDN != "" {
			// 邮箱路径：直接复用已解析 DN 做密码 bind，零二次搜索。
			ok, aerr = a.ad.authenticateWithPasswordByEntry(ru, rawPassword)
		} else {
			// 账号名路径：保持现逻辑（manager 搜索取 DN → 用户 DN+密码 bind）。
			ok, aerr = a.ad.AuthenticateWithPassword(username, rawPassword)
		}
	} else {
		if ru.userDN != "" {
			// 邮箱路径：直接用已解析的 UAC 判账号状态，无需再搜索。
			if ru.userAccountControl&0x2 != 0 || ru.userAccountControl&0x10 != 0 {
				return "", "", "", ErrUserNotFound
			}
			ok = true
		} else {
			// 账号名路径：目录查询模式。
			ok, aerr = a.ad.Authenticate(username)
		}
	}
	if aerr != nil {
		return "", "", "", aerr
	}
	if !ok {
		return "", "", "", ErrUserNotFound
	}

	// 7. 系统存在校验（用解析出的 sAMAccountName），取 HomeDir。
	u, uerr := user.Lookup(username)
	if uerr != nil {
		return "", "", "", ErrNotProvisioned
	}

	// 8. 签发一次性 token。
	tok, terr := a.token.Issue(username)
	if terr != nil {
		return "", "", "", ErrServiceUnavailable
	}
	return tok, username, u.HomeDir, nil
}

// resolveForLogin 解析登录标识为内部 resolvedUser。
// hasAt 由调用方已判定的标识类型决定；邮箱在此处做 AD 反查。
func (a *Authenticator) resolveForLogin(rawLogin string, hasAt bool) (*resolvedUser, error) {
	if hasAt {
		return a.ad.ResolveEmail(rawLogin)
	}
	return &resolvedUser{sAMAccountName: rawLogin}, nil
}

// ValidateAndConsume 校验并消费一次性 token，返回绑定的用户名与 HomeDir。
// 并对用户名做兜底校验（正则 + 非root + 系统存在），任一失败即拒绝。
// token 无效/过期/已消费返回 ErrInvalidToken。
func (a *Authenticator) ValidateAndConsume(token string) (username, homeDir string, err error) {
	username, ok := a.token.Consume(token)
	if !ok {
		return "", "", ErrInvalidToken
	}

	if username == "" || !usernameRegexp.MatchString(username) {
		return "", "", ErrInvalidFormat
	}
	if username == "root" {
		return "", "", ErrRootNotAllowed
	}
	u, uerr := user.Lookup(username)
	if uerr != nil {
		return "", "", ErrNotProvisioned
	}
	return username, u.HomeDir, nil
}
