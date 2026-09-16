package auth

import (
	"reflect"
	"strings"
	"testing"
)

func TestExtractEnvVars(t *testing.T) {
	cases := []struct {
		name    string
		content string
		want    []string
	}{
		{
			name:    "simple",
			content: `{"env": {"A": "${env.FOO_API_KEY}"}}`,
			want:    []string{"FOO_API_KEY"},
		},
		{
			name: "multiple preserve order",
			content: `{
				"b": "${env.B_API_KEY}",
				"a": "${env.A_API_KEY}"
			}`,
			want: []string{"B_API_KEY", "A_API_KEY"},
		},
		{
			name: "duplicates deduped",
			content: `{
				"x": "${env.ONE_API_KEY}",
				"y": "${env.ONE_API_KEY}"
			}`,
			want: []string{"ONE_API_KEY"},
		},
		{
			name: "line comment stripped",
			content: `{
				"a": "${env.REAL_API_KEY}",
				// "${env.SHOULD_NOT_MATCH_API_KEY}"
				"b": "${env.TWO_API_KEY}"
			}`,
			want: []string{"REAL_API_KEY", "TWO_API_KEY"},
		},
		{
			name: "block comment stripped",
			content: `{
				"a": "${env.REAL_API_KEY}",
				/* "${env.SHOULD_NOT_MATCH_API_KEY}" */
				"b": "${env.TWO_API_KEY}"
			}`,
			want: []string{"REAL_API_KEY", "TWO_API_KEY"},
		},
		{
			name: "string keeps block comment markers",
			content: `{
				"a": "${env.ONE_API_KEY}",
				"literal": "/* ${env.IN_STRING_API_KEY} */"
			}`,
			// 字符串内的 /* */ 不是注释，引用应被保留。
			want: []string{"ONE_API_KEY", "IN_STRING_API_KEY"},
		},
		{
			name: "url with double slash inside string",
			content: `{
				"api": "https://api.example.com/v1/${env.API_KEY}"
			}`,
			// URL 中的 // 位于字符串内，不应被当作行注释截断。
			want: []string{"API_KEY"},
		},
		{
			name:    "empty content",
			content: ``,
			want:    nil,
		},
		{
			name:    "empty object",
			content: `{}`,
			want:    nil,
		},
		{
			name:    "no reference",
			content: `{"a": "plain text", "b": 42}`,
			want:    nil,
		},
		{
			name:    "no env prefix",
			content: `{"a": "${OTHER.VAR}", "b": "${VAR}"}`,
			want:    nil,
		},
		{
			name:    "invalid var name ignored",
			content: `{"a": "${env.1BAD}", "b": "${env.ok_2}"}`,
			want:    []string{"ok_2"},
		},
		{
			name:    "opencode official brace syntax",
			content: `{"apiKey": "{env:YFENG_API_KEY}"}`,
			want:    []string{"YFENG_API_KEY"},
		},
		{
			name: "mixed brace and dollar syntax preserve doc order",
			content: `{
				"a": "{env:A_KEY}",
				"b": "${env.B_KEY}"
			}`,
			want: []string{"A_KEY", "B_KEY"},
		},
		{
			name: "same var in both syntaxes deduped",
			content: `{
				"a": "{env:ONE_KEY}",
				"b": "${env.ONE_KEY}"
			}`,
			want: []string{"ONE_KEY"},
		},
		{
			name:    "brace syntax invalid var name ignored",
			content: `{"a": "{env:1BAD}"}`,
			want:    nil,
		},
		{
			name:    "plain braces not matched",
			content: `{"a": "{some:thing}", "b": "x"}`,
			want:    nil,
		},
		{
			name: "brace syntax in comment stripped",
			content: `{
				"a": "{env:REAL_KEY}",
				// "{env:SHOULD_NOT_MATCH_KEY}"
				"b": "{env:TWO_KEY}"
			}`,
			want: []string{"REAL_KEY", "TWO_KEY"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ExtractEnvVars([]byte(tc.content))
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("ExtractEnvVars(%q) = %v, want %v", tc.content, got, tc.want)
			}
		})
	}
}

func TestMarshalAuthFileDeterministic(t *testing.T) {
	a := AuthFile{"B": "v2", "A": "v1", "C": "v3"}
	b := AuthFile{"C": "v3", "B": "v2", "A": "v1"} // 相同内容，map 顺序不同
	xa := marshalAuthFile(a)
	xb := marshalAuthFile(b)
	if string(xa) != string(xb) {
		t.Errorf("marshalAuthFile not deterministic: %q vs %q", xa, xb)
	}
	s := string(xa)
	for _, k := range []string{`"A"`, `"B"`, `"C"`} {
		if !strings.Contains(s, k) {
			t.Errorf("marshalAuthFile missing key %s: %s", k, xa)
		}
	}
}

func TestAuthFileMerged(t *testing.T) {
	base := AuthFile{"A": "v1", "B": "v2"}
	merged := base.merged(AuthFile{"A": "v3", "C": "v4"})

	if merged["A"] != "v3" {
		t.Errorf("merged A = %q, want v3 (overridden)", merged["A"])
	}
	if merged["B"] != "v2" {
		t.Errorf("merged B = %q, want v2 (preserved)", merged["B"])
	}
	if merged["C"] != "v4" {
		t.Errorf("merged C = %q, want v4 (added)", merged["C"])
	}
	// 原 map 不被修改。
	if base["A"] != "v1" {
		t.Errorf("base A mutated = %q, want v1", base["A"])
	}

	// 空值不覆盖已有值。
	merged2 := base.merged(AuthFile{"A": "", "D": "v5"})
	if merged2["A"] != "v1" {
		t.Errorf("merged2 A = %q, want v1 (empty value should not override)", merged2["A"])
	}
	if merged2["D"] != "v5" {
		t.Errorf("merged2 D = %q, want v5", merged2["D"])
	}
}

func TestExtractCodexEnvKeys(t *testing.T) {
	cases := []struct {
		name    string
		content string
		want    []string
	}{
		{
			name: "single env_key",
			content: `model = "deepseek-v4-flash-0731"
model_provider = "yfeng"
model_providers = {
  yfeng = {
    name = "yfeng",
    base_url = "https://example.com/v1",
    env_key = "YANFENG_API_KEY",
    wire_api = "chat",
  }
}`,
			want: []string{"YANFENG_API_KEY"},
		},
		{
			name: "multiple env_key preserve order",
			content: `[model_providers.a]
env_key = "AAA_API_KEY"
[model_providers.b]
env_key = "BBB_API_KEY"
[model_providers.c]
env_key = "AAA_API_KEY"`,
			want: []string{"AAA_API_KEY", "BBB_API_KEY"},
		},
		{
			name: "comment line excluded",
			content: `# env_key = "COMMENTED_API_KEY"
model_provider = "x"
[model_providers.x]
env_key = "REAL_API_KEY"`,
			want: []string{"REAL_API_KEY"},
		},
		{
			name:    "non env_key keys not matched",
			content: `[model_providers.x]\nname = "env_key"`,
			want:    nil,
		},
		{
			name: "invalid var name ignored",
			content: `[model_providers.x]
env_key = "1BAD"
env_key = "ok_2"`,
			// 非法名（以数字开头）不匹配；ok_2 合法。
			want: []string{"ok_2"},
		},
		{
			name:    "empty content",
			content: ``,
			want:    nil,
		},
		{
			name:    "no env_key",
			content: `[model_providers.x]\nname = "x"\nbase_url = "https://example.com"`,
			want:    nil,
		},
		{
			name:    "whitespace and indented lines",
			content: "\n  env_key = \"SPACED_API_KEY\"   \n",
			want:    []string{"SPACED_API_KEY"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ExtractCodexEnvKeys([]byte(tc.content))
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("ExtractCodexEnvKeys(%q) = %v, want %v", tc.content, got, tc.want)
			}
		})
	}
}

func TestRequiredCodexEnvVarsMissingFile(t *testing.T) {
	// 文件不存在/读取失败时返回 nil（不阻断登录）。
	// 使用空用户名/homeDir 或不可能存在的用户，保证不会触发真实 sudo 读取成功。
	got := RequiredCodexEnvVars("", "")
	if got != nil {
		t.Errorf("RequiredCodexEnvVars(empty) = %v, want nil", got)
	}
	// 无 sudo 环境时也应返回 nil（不阻断），不 panic。
	got = RequiredCodexEnvVars("definitely_no_such_user_xyz", "/nonexistent")
	if got != nil {
		t.Errorf("RequiredCodexEnvVars(no user) = %v, want nil", got)
	}
}
