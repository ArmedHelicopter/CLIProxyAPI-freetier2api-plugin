package core

// Configurable 由需要读取供应商专属配置的实例实现。
//
// 根层只解析全局键（enabled_realms / state_dir / …），其余原始键原样交给
// 各供应商自己解释：配置键的语义属于协议层，根层不该逐个知道。
//
// raw 是 key → 原始字符串（配置是逐行 key/value 解析的）；实现必须容忍
// 缺失键与 nil map，并保留既有默认值。
type Configurable interface {
	Configure(settings Settings, raw map[string]string)
}
