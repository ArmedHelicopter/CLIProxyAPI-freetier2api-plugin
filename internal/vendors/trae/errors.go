package trae

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"freetier2api-plugin/internal/core"
	"freetier2api-plugin/internal/upstream"
)

// 本文件实现 Trae 家族的上游错误分类。：解析 Trae 家族上游错误码。

type ErrorKind = upstream.Kind

const (
	KindCredential = upstream.KindCredential
	KindRateLimit  = upstream.KindRateLimit
	KindTransient  = upstream.KindTransient
	KindClient     = upstream.KindClient
)

type Error = upstream.Error

func Classify(status int, body string) error {
	var payload struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Msg     string `json:"msg"`
	}
	_ = json.Unmarshal([]byte(body), &payload)

	msg := core.FirstNonEmpty(payload.Message, payload.Msg, body)
	code := ""
	if payload.Code != 0 {
		code = intString(payload.Code)
	}

	switch {
	case status == http.StatusUnauthorized || payload.Code == 4001 || payload.Code == 4008:
		return &Error{Kind: KindCredential, Status: status, Code: code, Msg: msg}
	case payload.Code == 1005 || strings.Contains(strings.ToLower(msg), "credit") || strings.Contains(msg, "积分不足"):
		return &Error{Kind: KindCredential, Status: status, Code: "1005", Msg: "账号积分不足"}
	case status == http.StatusTooManyRequests || payload.Code == 429 || payload.Code == 9074:
		return &Error{Kind: KindRateLimit, Status: status, Code: code, Msg: msg}
	case status >= 500:
		return &Error{Kind: KindTransient, Status: status, Code: code, Msg: msg}
	default:
		return &Error{Kind: KindClient, Status: status, Code: code, Msg: msg}
	}
}

func intString(value int) string {
	if value == 0 {
		return ""
	}
	return fmt.Sprintf("%d", value)
}
