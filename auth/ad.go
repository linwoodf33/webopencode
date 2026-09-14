package auth

import (
	"crypto/tls"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/go-ldap/ldap/v3"
)

// 认证错误分类（sentinel error）。
var (
	// ErrServiceUnavailable AD 连接/绑定/超时等连接层错误（服务端故障）。
	// 对应 502/503，文案「authentication service unavailable」。
	ErrServiceUnavailable = errors.New("authentication service unavailable")
	// ErrUserNotFound 搜索无结果（用户不存在）或账号禁用/锁定。
	// 对应 401，文案「user not found or not authorized」。
	ErrUserNotFound = errors.New("user not found or not authorized")
)

// ADClient 封装与 AD(LDAPS) 的交互，采用「目录查询模式」：
// 用 manager 账号绑定 LDAPS，在 searchDN 下按 sAMAccountName 搜索，
// 只要存在一个有效账号即认证通过（不校验用户本人密码）。
type ADClient struct {
	server      string
	managerDN   string
	managerPass string
	searchDN    string
	tlsInsecure bool
	timeout     time.Duration
}

// NewADClient 基于配置创建一个 AD 客户端。
func NewADClient(cfg *Config) *ADClient {
	return &ADClient{
		server:      cfg.AD.Server,
		managerDN:   cfg.AD.ManagerDN,
		managerPass: cfg.AD.ManagerPassword,
		searchDN:    cfg.AD.SearchDN,
		tlsInsecure: cfg.AD.TLSInsecure,
		timeout:     cfg.ADTimeout,
	}
}

// dial 建立 LDAPS 连接并绑定 manager 账号。
// 失败时返回底层错误，调用方将其归类为服务端故障。
func (c *ADClient) dial() (*ldap.Conn, error) {
	conn, err := ldap.DialURL(
		"ldaps://"+c.server,
		ldap.DialWithTLSConfig(&tls.Config{
			InsecureSkipVerify: c.tlsInsecure, // #nosec G402 -- 按配置决定是否校验证书
		}),
	)
	if err != nil {
		return nil, fmt.Errorf("dial ldaps %s: %w", c.server, err)
	}
	if err := conn.Bind(c.managerDN, c.managerPass); err != nil {
		conn.Close()
		return nil, fmt.Errorf("bind manager: %w", err)
	}
	return conn, nil
}

// Authenticate 使用目录查询模式校验用户名是否存在且账号有效。
// 返回是否通过认证。
//   - 连接层错误（拨号/绑定/超时/搜索失败）返回 ErrServiceUnavailable。
//   - 用户不存在或账号禁用/锁定返回 ErrUserNotFound。
//
// 采用 fail-closed：AD 不可达时绝不降级放行。
func (c *ADClient) Authenticate(username string) (bool, error) {
	conn, err := c.dial()
	if err != nil {
		log.Printf("ad: connection/bind failed: %v", err)
		return false, ErrServiceUnavailable
	}
	defer conn.Close()

	// 搜索：必须转义用户名，防止 LDAP 注入。
	filter := "(&(objectClass=user)(sAMAccountName=" + ldap.EscapeFilter(username) + "))"
	searchReq := ldap.NewSearchRequest(
		c.searchDN,
		ldap.ScopeWholeSubtree,
		ldap.NeverDerefAliases,
		0, 0, false,
		filter,
		[]string{"sAMAccountName", "userAccountControl"},
		nil,
	)

	// 设置超时：绑定后对连接设置 deadline。
	if c.timeout > 0 {
		conn.SetTimeout(c.timeout)
	}
	res, err := conn.Search(searchReq)
	if err != nil {
		log.Printf("ad: search failed: %v", err)
		return false, ErrServiceUnavailable
	}

	if len(res.Entries) == 0 {
		return false, ErrUserNotFound
	}

	// 账号有效性判定：取第一个命中条目，读取 userAccountControl。
	entry := res.Entries[0]
	var uac int
	if v := entry.GetAttributeValue("userAccountControl"); v != "" {
		if _, err := fmt.Sscanf(v, "%d", &uac); err != nil {
			log.Printf("ad: parse userAccountControl failed: %v", err)
			return false, ErrServiceUnavailable
		}
	}
	// 0x2 = ACCOUNTDISABLE（禁用）；0x10 = LOCKOUT（锁定）。
	if uac&0x2 != 0 || uac&0x10 != 0 {
		return false, ErrUserNotFound
	}

	return true, nil
}

// ResolveEmail 用 manager 连接，在 searchDN 下按 mail 或 userPrincipalName 反查邮箱，
// 解析出对应的 sAMAccountName、userDN 与 userAccountControl（用于后续密码 bind 与系统存在校验）。
//
//   - 连接层错误（拨号/绑定/搜索失败）返回 ErrServiceUnavailable。
//   - 无命中（邮箱不存在）返回 ErrUserNotFound。
//
// 注意：邮箱 local-part（@ 前段）不一定等于 sAMAccountName，必须通过 AD 属性反查，
// 绝不取 @ 前段作为账号名。
func (c *ADClient) ResolveEmail(email string) (*resolvedUser, error) {
	conn, err := c.dial()
	if err != nil {
		log.Printf("ad: connection/bind failed: %v", err)
		return nil, ErrServiceUnavailable
	}
	defer conn.Close()

	escaped := ldap.EscapeFilter(email)
	filter := "(&(objectClass=user)(|(mail=" + escaped + ")(userPrincipalName=" + escaped + ")))"
	searchReq := ldap.NewSearchRequest(
		c.searchDN,
		ldap.ScopeWholeSubtree,
		ldap.NeverDerefAliases,
		0, 0, false,
		filter,
		[]string{"sAMAccountName", "distinguishedName", "userAccountControl"},
		nil,
	)

	if c.timeout > 0 {
		conn.SetTimeout(c.timeout)
	}
	res, err := conn.Search(searchReq)
	if err != nil {
		log.Printf("ad: search failed: %v", err)
		return nil, ErrServiceUnavailable
	}

	if len(res.Entries) == 0 {
		return nil, ErrUserNotFound
	}

	// 取第一个命中条目：读取 sAMAccountName / DN / UAC。
	entry := res.Entries[0]
	ru := &resolvedUser{sAMAccountName: entry.GetAttributeValue("sAMAccountName")}
	ru.userDN = entry.DN
	if ru.userDN == "" {
		ru.userDN = entry.GetAttributeValue("distinguishedName")
	}
	if v := entry.GetAttributeValue("userAccountControl"); v != "" {
		if _, err := fmt.Sscanf(v, "%d", &ru.userAccountControl); err != nil {
			log.Printf("ad: parse userAccountControl failed: %v", err)
			return nil, ErrServiceUnavailable
		}
	}
	return ru, nil
}

// bindAsUser 以「用户 DN + 密码」在给定连接上 bind，验证用户本人密码。
// bind 失败且错误码为 InvalidCredentials(49) 视为密码错误 → ErrUserNotFound；
// 其余（服务器临时故障等）视为 ErrServiceUnavailable。
// 安全注意：错误字符串中绝不包含密码明文。
func bindAsUser(conn *ldap.Conn, userDN, password string) error {
	if err := conn.Bind(userDN, password); err != nil {
		if ldap.IsErrorWithCode(err, ldap.LDAPResultInvalidCredentials) {
			return ErrUserNotFound
		}
		log.Printf("ad: user bind failed (service-level): %v", err)
		return ErrServiceUnavailable
	}
	return nil
}

// authenticateWithPasswordByEntry 复用已解析条目（已含 userDN/UAC）做「用户 DN+密码 bind」强认证。
// 不再进行二次搜索：直接使用传入条目携带的 DN 与账号状态。
func (c *ADClient) authenticateWithPasswordByEntry(ru *resolvedUser, password string) (bool, error) {
	conn, err := c.dial()
	if err != nil {
		log.Printf("ad: connection/bind failed: %v", err)
		return false, ErrServiceUnavailable
	}
	defer conn.Close()

	if ru.userDN == "" {
		log.Printf("ad: resolved entry for %q has no distinguishedName", ru.sAMAccountName)
		return false, ErrServiceUnavailable
	}
	if ru.userAccountControl&0x2 != 0 || ru.userAccountControl&0x10 != 0 {
		return false, ErrUserNotFound
	}

	if c.timeout > 0 {
		conn.SetTimeout(c.timeout)
	}
	if err := bindAsUser(conn, ru.userDN, password); err != nil {
		return false, err
	}
	return true, nil
}

// AuthenticateWithPassword 使用「manager 搜索取 DN → 用户 DN+密码 bind」的强认证流程。
// 两步均在同一个连接上进行。
//
//   - 连接层错误（拨号/manager 绑定/搜索失败）返回 ErrServiceUnavailable。
//   - 搜索无结果（用户不存在）返回 ErrUserNotFound。
//   - 用户 DN+密码 bind 失败：错误码为 InvalidCredentials(49) 视为密码错误（ErrUserNotFound），
//     其余（如服务器临时故障）视为 ErrServiceUnavailable。
//   - 账号禁用/锁定返回 ErrUserNotFound。
//
// 安全注意：错误字符串中绝不包含密码明文（bind 失败时仅包装底层错误，不拼接密码）。
func (c *ADClient) AuthenticateWithPassword(username, password string) (bool, error) {
	// 1. dial：用 manager 绑定（同一连接上后续再以用户 DN 重新 bind）。
	conn, err := c.dial()
	if err != nil {
		log.Printf("ad: connection/bind failed: %v", err)
		return false, ErrServiceUnavailable
	}
	defer conn.Close()

	// 2. 搜索取 DN 与账号状态。
	filter := "(&(objectClass=user)(sAMAccountName=" + ldap.EscapeFilter(username) + "))"
	searchReq := ldap.NewSearchRequest(
		c.searchDN,
		ldap.ScopeWholeSubtree,
		ldap.NeverDerefAliases,
		0, 0, false,
		filter,
		[]string{"distinguishedName", "sAMAccountName", "userAccountControl"},
		nil,
	)

	if c.timeout > 0 {
		conn.SetTimeout(c.timeout)
	}
	res, err := conn.Search(searchReq)
	if err != nil {
		log.Printf("ad: search failed: %v", err)
		return false, ErrServiceUnavailable
	}

	if len(res.Entries) == 0 {
		return false, ErrUserNotFound
	}

	// 取第一个命中条目：读取 DN 与 UAC。
	entry := res.Entries[0]
	userDN := entry.DN
	if userDN == "" {
		userDN = entry.GetAttributeValue("distinguishedName")
	}
	if userDN == "" {
		log.Printf("ad: entry for %q has no distinguishedName", username)
		return false, ErrServiceUnavailable
	}

	var uac int
	if v := entry.GetAttributeValue("userAccountControl"); v != "" {
		if _, err := fmt.Sscanf(v, "%d", &uac); err != nil {
			log.Printf("ad: parse userAccountControl failed: %v", err)
			return false, ErrServiceUnavailable
		}
	}
	// 0x2 = ACCOUNTDISABLE（禁用）；0x10 = LOCKOUT（锁定）。
	if uac&0x2 != 0 || uac&0x10 != 0 {
		return false, ErrUserNotFound
	}

	// 3. 在同一连接上以「用户 DN + 密码」bind，验证用户本人密码。
	if err := bindAsUser(conn, userDN, password); err != nil {
		return false, err
	}

	return true, nil
}
