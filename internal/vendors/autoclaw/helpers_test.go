package autoclaw

import "encoding/json"

// jsonUnmarshalHelper 供测试把 JSON 文本解成 map。
func jsonUnmarshalHelper(raw string, target *map[string]any) error {
	return json.Unmarshal([]byte(raw), target)
}
