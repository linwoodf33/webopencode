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
