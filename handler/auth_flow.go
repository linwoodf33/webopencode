package handler

import (
	"errors"
	"net"
	"time"

	"github.com/gorilla/websocket"

	"webshell/auth"
	"webshell/protocol"
)

// api_key 补全流程的错误 sentinel。
var (
	// ErrAuthCanceled 用户取消了 api_key 补全。
	ErrAuthCanceled = errors.New("auth canceled by user")
	// ErrAuthTimeout api_key 补全等待用户输入超时。
	ErrAuthTimeout = errors.New("auth request timeout")
)

// defaultAuthTimeout 未配置 AuthTimeout 时的默认补全等待超时。
var defaultAuthTimeout = time.Duration(auth.DefaultAuthTimeout) * time.Second

// collectAPIKeys 在 WebSocket 连接上执行 api_key 补全流程。
//
// 下发 auth-request(envVars=missing) -> 等待客户端逐条返回 auth-response /
// auth-cancel（用 SetReadDeadline 实现超时）。对每个合法响应写回 .auth.json
// （幂等合并）并记录。返回补齐的 {envVar: apiKey} map。
//
// 安全约束：
//   - 只接受 envVar 在 missing 列表内的响应（防客户端注入任意变量名/覆盖其它 key）；
//   - api_key 仅通过 WebSocket 消息传输并写回文件，绝不进日志/命令行参数；
//   - 调用方必须保证本流程在 hubSession 创建/readLoop 启动之前执行（直接读写 conn）。
//
// 用户取消返回 ErrAuthCanceled，等待超时返回 ErrAuthTimeout，其余错误原样返回。
func collectAPIKeys(conn *websocket.Conn, username, homeDir string,
	envVars []string, timeout time.Duration) (map[string]string, error) {

	if len(envVars) == 0 {
		return map[string]string{}, nil
	}
	if timeout <= 0 {
		timeout = defaultAuthTimeout
	}

	// 去重保序，并建立待补全集合（防注入白名单 + 已补齐判定）。
	var missing []string
	pending := make(map[string]bool, len(envVars))
	for _, v := range envVars {
		if !pending[v] {
			missing = append(missing, v)
			pending[v] = true
		}
	}

	// 下发 auth-request，列出缺失变量与超时秒数。
	payload, err := protocol.AuthRequestMessage(missing, int(timeout/time.Second)).Marshal()
	if err != nil {
		return nil, err
	}
	if err := conn.WriteMessage(websocket.TextMessage, payload); err != nil {
		return nil, err
	}

	result := make(map[string]string, len(missing))
	deadline := time.Now().Add(timeout)

	for len(missing) > 0 {
		// 每次读取前刷新读截止时间，保证整体等待不超过 timeout。
		if err := conn.SetReadDeadline(deadline); err != nil {
			return nil, err
		}
		_, raw, err := conn.ReadMessage()
		if err != nil {
			var netErr net.Error
			if errors.As(err, &netErr) && netErr.Timeout() {
				return nil, ErrAuthTimeout
			}
			return nil, err
		}

		msg, err := protocol.Parse(raw)
		if err != nil {
			continue // 忽略无法解析的消息
		}
		switch msg.Type {
		case protocol.TypeAuthCancel:
			return nil, ErrAuthCanceled
		case protocol.TypeAuthResponse:
			resp, ok := msg.ParseAuthResponse()
			if !ok {
				continue
			}
			// 防注入：只接受「请求缺失列表内」且「尚未补齐」的变量（apiKey 非空）。
			if !pending[resp.EnvVar] || resp.APIKey == "" {
				continue
			}
			// 写回 .auth.json（幂等合并），失败则终止流程。
			if err := auth.WriteAuthFile(username, homeDir, auth.AuthFile{resp.EnvVar: resp.APIKey}); err != nil {
				return nil, err
			}
			result[resp.EnvVar] = resp.APIKey
			delete(pending, resp.EnvVar)
			// 从待补全列表中移除该变量。
			rest := missing[:0]
			for _, v := range missing {
				if v != resp.EnvVar {
					rest = append(rest, v)
				}
			}
			missing = rest
		default:
			// 其余消息类型与补全流程无关，忽略。
		}
	}

	// 清理读截止时间，避免影响后续 attach/readLoop 的正常读取。
	_ = conn.SetReadDeadline(time.Time{})
	return result, nil
}

// resolveAuthEnv 汇总 AI 代理（opencode/codex）启动所需的环境变量（api_key）。
//
// 流程：读取配置文件（opencode.jsonc / config.toml）的 env 引用列表 -> 读
// .auth.json 已有 key -> 计算缺失项 -> 缺失时经 collectAPIKeys 与用户交互补全 ->
// 返回 {envVar: apiKey} map（含已有与新增）。
//
// requiredVars 参数化「env 引用来源」：opencode 传 auth.RequiredEnvVars，
// codex 传 auth.RequiredCodexEnvVars。其余逻辑（collectAPIKeys、防注入白名单、
// ~/.auth.json 幂等写回、超时清理读 deadline）与 mode 无关，完全复用。
// 未进入代理模式（requiredVars 为 nil）或配置无 env 引用时返回 (nil, nil)。
func resolveAuthEnv(conn *websocket.Conn, username, homeDir string,
	requiredVars func(string, string) []string, timeout time.Duration) (map[string]string, error) {
	if requiredVars == nil {
		return nil, nil
	}
	envVars := requiredVars(username, homeDir)
	if len(envVars) == 0 {
		return nil, nil
	}

	existing, err := auth.ReadAuthFile(username, homeDir)
	if err != nil {
		return nil, err
	}

	missing := make([]string, 0, len(envVars))
	for _, v := range envVars {
		if existing[v] == "" {
			missing = append(missing, v)
		}
	}
	if len(missing) > 0 {
		if _, err := collectAPIKeys(conn, username, homeDir, missing, timeout); err != nil {
			return nil, err
		}
		// 补全后重新读取（collectAPIKeys 已写回 .auth.json）。
		existing, err = auth.ReadAuthFile(username, homeDir)
		if err != nil {
			return nil, err
		}
	}

	authEnv := make(map[string]string, len(envVars))
	for _, v := range envVars {
		if existing[v] != "" {
			authEnv[v] = existing[v]
		}
	}
	return authEnv, nil
}
