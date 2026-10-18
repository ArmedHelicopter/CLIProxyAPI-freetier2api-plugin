package opencodezen

import (
	"regexp"
	"testing"
)

// 本文件锁定 ZEN session ID 的编码规则。
//
// 这段编码是免费层的硬门槛：早期实现用随机十六进制填时间部分，形态对但
// 编码错，上游一律 403 FreeTierError，且错误信息完全看不出是编码问题。
// 因此这里对「形态」和「时间部分编码」分别断言。

func TestSessionIDShape(t *testing.T) {
	body := []byte(`{"model":"deepseek-v4.1-flash-free","messages":[{"role":"user","content":"hi"}]}`)
	id := sessionIDFor(body)

	if !canonicalSessionPattern.MatchString(id) {
		t.Fatalf("session %q 不符合 canonical 形态", id)
	}
	if !descendingTimePartPattern.MatchString(id[4:16]) {
		t.Fatalf("session %q 的时间部分 %q 不是 descending 编码（免费层会 403）", id, id[4:16])
	}
	// 随机信号也必须落在同一形态内（对话取不到种子时的兜底路径）。
	if got := sessionIDFor([]byte(`{"messages":[]}`)); !canonicalSessionPattern.MatchString(got) ||
		!descendingTimePartPattern.MatchString(got[4:16]) {
		t.Fatalf("兜底 session %q 不合规", got)
	}
	// 非 JSON 请求体同样不能 panic，且要产出合规 session。
	if got := sessionIDFor([]byte("not json")); !canonicalSessionPattern.MatchString(got) ||
		!descendingTimePartPattern.MatchString(got[4:16]) {
		t.Fatalf("非 JSON 请求体的 session %q 不合规", got)
	}
}

// TestSessionIDStablePerConversation 验证同一段对话得到同一个 session。
//
// 用整段历史做种子的话，多轮对话每轮都会换 session，上游 prompt cache
// 永远命中不了；这里钉住「只看首条 user 消息」。
func TestSessionIDStablePerConversation(t *testing.T) {
	first := []byte(`{"messages":[{"role":"user","content":"讲个笑话"},{"role":"assistant","content":"好的"}]}`)
	grown := []byte(`{"messages":[{"role":"user","content":"讲个笑话"},{"role":"assistant","content":"好的"},` +
		`{"role":"user","content":"再来一个"},{"role":"assistant","content":"好"}]}`)
	if sessionIDFor(first) != sessionIDFor(grown) {
		t.Fatal("同一段对话在历史增长后 session 变了，prompt cache 会失效")
	}

	other := []byte(`{"messages":[{"role":"user","content":"讲个别的"}]}`)
	if sessionIDFor(first) == sessionIDFor(other) {
		t.Fatal("不同对话得到了相同 session")
	}
}

// TestCanonicalSessionIDPreservesOfficial 验证已经是官方铸造的 session 原样保留。
func TestCanonicalSessionIDPreservesOfficial(t *testing.T) {
	// descendingTimePart 对任意哈希都产出以 ffe 收尾的时间部分。
	official := canonicalSessionID("any-signal")
	if got := canonicalSessionID(official); got != official {
		t.Fatalf("官方形态的 session 被重编码了：%q -> %q", official, got)
	}
	// 形态对但编码错（随机十六进制）时必须重编码，否则上游 403。
	randomHex := "ses_" + "0123456789ab" + "ABCDEFGHIJKLMN"
	if !canonicalSessionPattern.MatchString(randomHex) {
		t.Fatal("测试样本本身不合法")
	}
	if got := canonicalSessionID(randomHex); !descendingTimePartPattern.MatchString(got[4:16]) {
		t.Fatalf("随机时间部分未被重编码：%q", got)
	}
}

// TestDescendingTimePartEndsWithFFE 验证时间部分的低 12 位落在 ffe。
func TestDescendingTimePartEndsWithFFE(t *testing.T) {
	pattern := regexp.MustCompile(`^[0-9a-f]{9}ffe$`)
	for _, seed := range []string{"a", "b", "hello world", "\x00\x01", "讲个笑话"} {
		id := canonicalSessionID(seed)
		if !pattern.MatchString(id[4:16]) {
			t.Errorf("seed %q 的时间部分 %q 未以 ffe 收尾", seed, id[4:16])
		}
	}
}
