package auth

import (
	"bytes"
	"encoding/json"
	"errors"
	"os/exec"
	"regexp"
	"sort"
	"strings"

	"github.com/tidwall/jsonc"
)

// AuthFile 是 ~/.auth.json 的映射：{环境变量名: api_key}。
type AuthFile map[string]string

// DefaultAuthFileName 用户 api_key 存储文件名（位于用户家目录）。
const DefaultAuthFileName = ".auth.json"

// authEnvVarRe 匹配 opencode.jsonc 中的旧式 ${env.VAR} 引用（兼容历史配置）。
// 变量名限定为 C 标识符风格，防注入。
var authEnvVarRe = regexp.MustCompile(`\$\{env\.([A-Za-z_][A-Za-z0-9_]*)\}`)

// envBraceRe 匹配 opencode 官方语法 {env:VAR}（见 opencode 文档 "Env vars" 章节）。
// 只匹配 {env:...} 前缀，不误伤其它花括号内容；旧式 `${env.VAR}` 前缀为 `${env.`
// （花括号内是点），与本正则的 `{env:`（冒号）在字符层面天然互斥，不会被重复匹配；
// 合并结果另有去重兜底。变量名限定为 C 标识符风格，防注入。
var envBraceRe = regexp.MustCompile(`\{env:([A-Za-z_][A-Za-z0-9_]*)\}`)

// defaultAuthFile 返回用户家目录下的 .auth.json 绝对路径。
func defaultAuthFile(homeDir string) string {
	return homeDir + "/" + DefaultAuthFileName
}

// readFileAsUser 以目标用户身份读取文件内容（经 sudo -u <user> cat）。
// 用于读取服务进程（ease）无权限访问的用户家目录内文件。
func readFileAsUser(username, path string) ([]byte, error) {
	sudoPath, err := exec.LookPath("sudo")
	if err != nil {
		return nil, errors.New("cannot find sudo: " + err.Error())
	}
	cmd := exec.Command(sudoPath, "-n", "-u", username, "cat", path)
	cmd.Dir = "/"
	return cmd.Output()
}

// ReadAuthFile 读取用户家目录 ~/.auth.json（经 sudo -u <user> cat）。
// 文件不存在时返回 (nil, nil) 不报错（首登场景）；解析失败返回 error。
// username/homeDir 必须已完成系统存在性校验。
func ReadAuthFile(username, homeDir string) (AuthFile, error) {
	if username == "" || homeDir == "" {
		return nil, errors.New("empty username or homeDir")
	}
	sudoPath, err := exec.LookPath("sudo")
	if err != nil {
		return nil, errors.New("cannot find sudo: " + err.Error())
	}

	path := defaultAuthFile(homeDir)
	cmd := exec.Command(sudoPath, "-n", "-u", username, "cat", path)
	cmd.Dir = "/"
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		// cat 对不存在的文件退出码为 1 且 stderr 含 "No such file"。
		// 首登场景无 .auth.json 属正常，返回 (nil,nil) 不阻断。
		msg := strings.TrimSpace(stderr.String())
		if strings.Contains(msg, "No such file") || strings.Contains(msg, "no such file") {
			return nil, nil
		}
		return nil, err
	}

	var m AuthFile
	if err := json.Unmarshal(out, &m); err != nil {
		return nil, err
	}
	if m == nil {
		m = AuthFile{}
	}
	return m, nil
}

// WriteAuthFile 写入用户家目录 ~/.auth.json（幂等合并）。
// 经 `sudo -u <user> bash -c 'umask 077; cat > <dest> && chmod 600 <dest>'` 执行，
// 内容经 stdin 传入（api_key 不进命令行参数/进程列表）。
// 写前会先读取现有文件，与 data 合并后整体写回，保证多变量补全互不覆盖。
// username/homeDir 必须已完成系统存在性校验。
func WriteAuthFile(username, homeDir string, data AuthFile) error {
	if username == "" || homeDir == "" {
		return errors.New("empty username or homeDir")
	}
	if len(data) == 0 {
		return nil
	}

	// 幂等合并：先读现有文件再合入本次数据。
	existing, err := ReadAuthFile(username, homeDir)
	if err != nil {
		return err
	}
	merged := existing.merged(data)

	sudoPath, err := exec.LookPath("sudo")
	if err != nil {
		return errors.New("cannot find sudo: " + err.Error())
	}

	dest := defaultAuthFile(homeDir)
	// umask 077 确保创建的文件仅属主可读写；chmod 600 兜底。内容经 stdin 注入，
	// 避免 api_key 出现在 sudo/bash 命令行参数（ps 可被其他用户读到）。
	cmd := exec.Command(sudoPath, "-n", "-u", username, "bash", "-c",
		"umask 077; cat > "+shellQuote(dest)+" && chmod 600 "+shellQuote(dest))
	cmd.Dir = "/"
	cmd.Stdin = bytes.NewReader(marshalAuthFile(merged))
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return errors.New(msg)
	}
	return nil
}

// merged 将 with 合入 a，返回新 map（不修改 a）。with 中值为空的项不覆盖。
func (a AuthFile) merged(with AuthFile) AuthFile {
	out := AuthFile{}
	for k, v := range a {
		out[k] = v
	}
	for k, v := range with {
		if v != "" {
			out[k] = v
		}
	}
	return out
}

// marshalAuthFile 将 AuthFile 序列化为确定性 JSON（键排序），保证幂等合并后
// 文件内容稳定（不因 map 遍历随机顺序产生无意义 diff）。
func marshalAuthFile(data AuthFile) []byte {
	keys := make([]string, 0, len(data))
	for k := range data {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var sb strings.Builder
	sb.WriteString("{")
	for i, k := range keys {
		if i > 0 {
			sb.WriteString(",")
		}
		kb, _ := json.Marshal(k)
		vb, _ := json.Marshal(data[k])
		sb.Write(kb)
		sb.WriteString(":")
		sb.Write(vb)
	}
	sb.WriteString("}\n")
	return []byte(sb.String())
}

// ExtractEnvVars 从 opencode.jsonc 内容中提取所有引用的环境变量名
// （同时支持 opencode 官方语法 {env:VAR} 与旧式 ${env.VAR}，按文档出现顺序去重保序）。
// 先剥离 JSONC 注释（// 与 /* */，字符串内的不剥离），避免
// 注释/URL 中的引用被误判。
func ExtractEnvVars(content []byte) []string {
	cleaned := jsonc.ToJSON(content)
	type pos struct {
		idx  int
		name string
	}
	var found []pos
	collect := func(re *regexp.Regexp) {
		for _, m := range re.FindAllSubmatchIndex(cleaned, -1) {
			// m[0:2] 为整段匹配，m[2:4] 为捕获组（变量名）。
			found = append(found, pos{idx: m[0], name: string(cleaned[m[2]:m[3]])})
		}
	}
	collect(authEnvVarRe)
	collect(envBraceRe)
	// 按文档出现位置排序，保证两种语法混合时仍保序。
	sort.Slice(found, func(i, j int) bool { return found[i].idx < found[j].idx })

	seen := make(map[string]bool, len(found))
	var out []string
	for _, f := range found {
		if !seen[f.name] {
			seen[f.name] = true
			out = append(out, f.name)
		}
	}
	return out
}

// RequiredEnvVars 读取用户 ~/.config/opencode/opencode.jsonc 并提取其引用的
// 环境变量名。文件缺失/读取失败/解析失败时返回 nil（不阻断登录，由调用方决定
// 是否触发补全流程）。
func RequiredEnvVars(username, homeDir string) []string {
	if username == "" || homeDir == "" {
		return nil
	}
	content, err := readFileAsUser(username, homeDir+"/.config/opencode/opencode.jsonc")
	if err != nil {
		return nil
	}
	return ExtractEnvVars(content)
}

// shellQuote 将字符串包进单引号，供拼接到 bash -c 命令行使用（规避特殊字符注入）。
// 与 pty 包的同名辅助保持一致的语义；auth 包不能反向依赖 pty，故在此独立实现。
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
