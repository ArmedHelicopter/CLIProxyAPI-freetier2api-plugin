package app

// 本文件实现 response.normalize_before 的入参解析。
//
// 宿主对每一条 SSE 行都调用一次该钩子，而 RPC 参数里带着完整客户端请求
// （OriginalRequest）与完整上游请求（TranslatedRequest）——长上下文下
// 各自可达数 MB，且都以 base64 出现在 JSON 里。插件真正需要的只有
// Stream / FromFormat / ToFormat / Body，所以这里用一个只认顶层对象的
// 扫描器取值：两个大字段只做字符串跳过，绝不 base64 解码、绝不分配。
// 整包 json.Unmarshal 会对这两份大字段做 base64 解码，那就是 issue #9
// 里 73% CPU 的来源。
//
// 扫描器只依赖 JSON 语法本身，不假设字段顺序、不假设有无外层包装；
// 任何结构异常都交给 decodeResponseTransform 用 encoding/json 兜底，
// 宁可慢一次，也不误判成「不需要处理」——后者会让 Claude 的流式
// 转换器因缺少 data: 前缀而丢帧（issue #5 / #8）。

import (
	"bytes"
	"encoding/base64"
)

// responseTransformFields 是 normalize_before 实际会用到的字段。
type responseTransformFields struct {
	fromFormat string
	toFormat   string
	stream     bool
	body       []byte
}

// normalizeResponseNeeded 是「是否给该分片补 data: 信封」的唯一判据。
//
// 同格式 OpenAI 响应由宿主 handler 自己加信封，提前包装会变成 data: data:；
// 只有跨协议流式转换时，转换器才需要一个带 data: 前缀的 SSE 帧。
func normalizeResponseNeeded(stream bool, fromFormat, toFormat string) bool {
	return stream && isChatFormat(fromFormat) && toFormat != "" && !isChatFormat(toFormat)
}

// normalizeResponseChunk 返回需要归一化时的分片 Body。
//
// ok=false 表示「本插件不处理」：宿主对该结果的语义就是保留原 body
// （callResponseNormalizer 在 err != nil 或 Body 为空时按 no-op 处理）。
func normalizeResponseChunk(request []byte) ([]byte, bool) {
	fields, okScan := scanResponseTransform(request)
	if !okScan {
		fields, okScan = decodeResponseTransform(request)
		if !okScan {
			return nil, false
		}
	}
	if !normalizeResponseNeeded(fields.stream, fields.fromFormat, fields.toFormat) {
		return nil, false
	}
	return fields.body, len(fields.body) > 0
}

// decodeResponseTransform 是扫描器的兜底：用窄结构体让 encoding/json 解码。
//
// 窄结构体不含 OriginalRequest / TranslatedRequest，encoding/json 跳过
// 未声明字段时不会 base64 解码它们，因此这里没有大块分配，只是多一次
// 全文 tokenize。
func decodeResponseTransform(request []byte) (responseTransformFields, bool) {
	var decoded struct {
		FromFormat string
		ToFormat   string
		Stream     bool
		Body       []byte
	}
	if errDecode := decodeRequest(request, &decoded); errDecode != nil {
		return responseTransformFields{}, false
	}
	return responseTransformFields{
		fromFormat: decoded.FromFormat,
		toFormat:   decoded.ToFormat,
		stream:     decoded.Stream,
		body:       decoded.Body,
	}, true
}

// scanResponseTransform 单次线性扫描顶层 JSON 对象，只解出需要的字段。
//
// 一旦三个标量齐全且判定为「不需要处理」，立刻返回——不往后扫两个大字段，
// 常见路径因此只读前一两百字节。判定为需要时继续扫到 Body；被跳过的大
// 字符串用 bytes.IndexByte 定位结束引号（memchr 速度），逐个字节的状态机
// 不会碰它们。
//
// ok=false 表示结构不符合预期（非对象、截断、出现转义等），调用方必须走
// decodeResponseTransform 兜底。
func scanResponseTransform(raw []byte) (responseTransformFields, bool) {
	cursor := jsonCursor{raw: raw}
	if !cursor.consume('{') {
		return responseTransformFields{}, false
	}
	var fields responseTransformFields
	var seen uint8
	for {
		if cursor.consume('}') {
			return fields, true
		}
		key, okKey := cursor.readString()
		if !okKey || !cursor.consume(':') {
			return responseTransformFields{}, false
		}
		if okField := fields.assign(key, &cursor, &seen); !okField {
			return responseTransformFields{}, false
		}

		// 三个标量齐全即可判定：不需要处理时无需继续扫后面的大字段。
		if seen&seenScalars == seenScalars {
			if !normalizeResponseNeeded(fields.stream, fields.fromFormat, fields.toFormat) {
				return fields, true
			}
			if seen&seenBody != 0 {
				return fields, true
			}
		}

		if cursor.consume('}') {
			return fields, true
		}
		if !cursor.consume(',') {
			return responseTransformFields{}, false
		}
	}
}

const (
	seenFromFormat uint8 = 1 << iota
	seenToFormat
	seenStream
	seenBody
	seenScalars = seenFromFormat | seenToFormat | seenStream
)

// assign 按 key 取值；未知 key 只跳过其值。
func (f *responseTransformFields) assign(key []byte, cursor *jsonCursor, seen *uint8) bool {
	switch {
	case bytes.Equal(key, []byte("FromFormat")):
		value, ok := cursor.readString()
		if !ok {
			return false
		}
		f.fromFormat = string(value)
		*seen |= seenFromFormat
		return true
	case bytes.Equal(key, []byte("ToFormat")):
		value, ok := cursor.readString()
		if !ok {
			return false
		}
		f.toFormat = string(value)
		*seen |= seenToFormat
		return true
	case bytes.Equal(key, []byte("Stream")):
		value, ok := cursor.readBool()
		if !ok {
			return false
		}
		f.stream = value
		*seen |= seenStream
		return true
	case bytes.Equal(key, []byte("Body")):
		encoded, ok := cursor.readString()
		if !ok {
			return false
		}
		decoded, okDecode := decodeBase64(encoded)
		if !okDecode {
			return false
		}
		f.body = decoded
		*seen |= seenBody
		return true
	default:
		return cursor.skipValue()
	}
}

// decodeBase64 解码 wire 上的 Body 字段。
//
// 只有判定为「需要补信封」之后才会走到，所以代价随分片大小而不是请求体
// 大小增长。
func decodeBase64(encoded []byte) ([]byte, bool) {
	decoded := make([]byte, base64.StdEncoding.DecodedLen(len(encoded)))
	n, errDecode := base64.StdEncoding.Decode(decoded, encoded)
	if errDecode != nil {
		return nil, false
	}
	return decoded[:n], true
}

// jsonCursor 是只够解析本协议载荷的最小游标。
type jsonCursor struct {
	raw []byte
	pos int
}

func (c *jsonCursor) skipSpace() {
	for c.pos < len(c.raw) {
		switch c.raw[c.pos] {
		case ' ', '\t', '\n', '\r':
			c.pos++
		default:
			return
		}
	}
}

func (c *jsonCursor) peek() (byte, bool) {
	c.skipSpace()
	if c.pos < len(c.raw) {
		return c.raw[c.pos], true
	}
	return 0, false
}

func (c *jsonCursor) consume(want byte) bool {
	ch, ok := c.peek()
	if !ok || ch != want {
		return false
	}
	c.pos++
	return true
}

// readString 读取一个不含转义的字符串，返回去引号后的原始字节。
//
// 需要取值的字段（格式名、base64 的 Body）都是纯 ASCII。出现转义即视为
// 结构异常交给 encoding/json 兜底，而不是在这里半懂不懂地解码。
func (c *jsonCursor) readString() ([]byte, bool) {
	if !c.consume('"') {
		return nil, false
	}
	start := c.pos
	offset := bytes.IndexByte(c.raw[start:], '"')
	if offset < 0 {
		return nil, false
	}
	end := start + offset
	if bytes.IndexByte(c.raw[start:end], '\\') >= 0 {
		return nil, false
	}
	c.pos = end + 1
	return c.raw[start:end], true
}

func (c *jsonCursor) readBool() (bool, bool) {
	if c.hasPrefix("true") {
		c.pos += len("true")
		return true, true
	}
	if c.hasPrefix("false") {
		c.pos += len("false")
		return false, true
	}
	return false, false
}

func (c *jsonCursor) hasPrefix(literal string) bool {
	c.skipSpace()
	if len(c.raw)-c.pos < len(literal) {
		return false
	}
	return bytes.Equal(c.raw[c.pos:c.pos+len(literal)], []byte(literal))
}

// skipValue 跳过一个值，不解析其内容。
func (c *jsonCursor) skipValue() bool {
	ch, ok := c.peek()
	if !ok {
		return false
	}
	switch ch {
	case '"':
		return c.skipString()
	case '{', '[':
		return c.skipContainer()
	default:
		return c.skipLiteral()
	}
}

// skipString 跳过一个字符串值（允许转义）。
func (c *jsonCursor) skipString() bool {
	if !c.consume('"') {
		return false
	}
	for c.pos < len(c.raw) {
		offset := bytes.IndexByte(c.raw[c.pos:], '"')
		if offset < 0 {
			return false
		}
		index := c.pos + offset
		// 数前导反斜杠：奇数个说明这个引号是被转义的，继续找。
		escapes := 0
		for i := index - 1; i >= 0 && c.raw[i] == '\\'; i-- {
			escapes++
		}
		c.pos = index + 1
		if escapes%2 == 0 {
			return true
		}
	}
	return false
}

// skipContainer 按括号深度跳过对象或数组。
func (c *jsonCursor) skipContainer() bool {
	depth := 0
	for c.pos < len(c.raw) {
		switch c.raw[c.pos] {
		case '"':
			if !c.skipString() {
				return false
			}
		case '{', '[':
			depth++
			c.pos++
		case '}', ']':
			depth--
			c.pos++
			if depth == 0 {
				return true
			}
		default:
			c.pos++
		}
	}
	return false
}

// skipLiteral 跳过一个数字 / true / false / null。
func (c *jsonCursor) skipLiteral() bool {
	start := c.pos
	for c.pos < len(c.raw) {
		switch c.raw[c.pos] {
		case ',', '}', ']', ' ', '\t', '\n', '\r':
			return c.pos > start
		}
		c.pos++
	}
	return c.pos > start
}
