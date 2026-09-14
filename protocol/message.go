package protocol

import (
	"encoding/base64"
	"encoding/json"
)

// MessageType 定义消息类型常量。
type MessageType string

const (
	// TypeInput 客户端 -> 服务端，写入 pty 的输入字节。
	TypeInput MessageType = "input"
	// TypeResize 客户端 -> 服务端，设置终端尺寸。
	TypeResize MessageType = "resize"
	// TypeOutput 服务端 -> 客户端，bash 输出字节。
	TypeOutput MessageType = "output"
	// TypeClose 服务端 -> 客户端，会话结束/错误。
	TypeClose MessageType = "close"
	// TypeSession 服务端 -> 客户端，当前会话标识（用于页面刷新后恢复同一终端）。
	TypeSession MessageType = "session"
)

// SessionData 会话标识数据。
type SessionData struct {
	ID string `json:"id"`
}

// ResizeData 终端尺寸数据。
type ResizeData struct {
	Cols uint16 `json:"cols"`
	Rows uint16 `json:"rows"`
}

// CloseData 关闭原因数据。
type CloseData struct {
	Reason string `json:"reason"`
}

// Message 是 WebSocket 上传递的 JSON 信封。
type Message struct {
	Type MessageType     `json:"type"`
	Data json.RawMessage `json:"data,omitempty"`
}

// Parse 解析一个 JSON 字节串为 Message。
func Parse(raw []byte) (*Message, error) {
	var m Message
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

// ParseResize 从 Message 的 Data 中解析尺寸。
// 若 cols/rows 非正整数则返回 false（调用方应忽略）。
func (m *Message) ParseResize() (ResizeData, bool) {
	var d ResizeData
	if len(m.Data) == 0 {
		return d, false
	}
	if err := json.Unmarshal(m.Data, &d); err != nil {
		return d, false
	}
	if d.Cols == 0 || d.Rows == 0 {
		return d, false
	}
	return d, true
}

// ParseInput 从 Message 的 Data 中解析输入字符串，返回真正的 UTF-8 字节。
// 客户端发送的 data 是 JSON 字符串（例如 {"type":"input","data":"echo hi\n"}），
// 因此 msg.Data 是经过 JSON 字符串编码的原始字节（含两端引号且 \n 是字面
// 反斜杠+n）。这里通过 json.Unmarshal 到 string 得到真实的字符序列（含真实
// 换行、退格、转义序列），供原样写入 pty。data 缺省时返回 nil。
func (m *Message) ParseInput() []byte {
	var s string
	if len(m.Data) == 0 {
		return nil
	}
	if err := json.Unmarshal(m.Data, &s); err != nil {
		return nil
	}
	return []byte(s)
}

// OutputMessage 构造一个 output 消息。
// pty 的原始输出字节可能包含 \x1b 颜色转义、\r、\n 等控制字节，直接放进
// JSON 字符串会引发编码问题。这里将字节安全地 base64 编码成字符串再放入
// message，前端收到后用 atob 解码回二进制即可，彻底规避控制字符问题。
func OutputMessage(data []byte) *Message {
	return &Message{Type: TypeOutput, Data: []byte("\"" + base64.StdEncoding.EncodeToString(data) + "\"")}
}

// CloseMessage 构造一个 close 消息。
func CloseMessage(reason string) *Message {
	raw, _ := json.Marshal(CloseData{Reason: reason})
	return &Message{Type: TypeClose, Data: raw}
}

// SessionMessage 构造一个 session 消息（携带会话标识）。
func SessionMessage(id string) *Message {
	raw, _ := json.Marshal(SessionData{ID: id})
	return &Message{Type: TypeSession, Data: raw}
}

// Marshal 序列化消息为 JSON 字节串。
func (m *Message) Marshal() ([]byte, error) {
	return json.Marshal(m)
}
