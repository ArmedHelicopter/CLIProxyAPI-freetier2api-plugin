package main

// 本文件是宿主回调的 CGO 实现（唯一必须留在 main 的 host 层代码）：
// //export/import "C" 只能出现在 main 包，因此这里只做「marshal → C 调用 →
// 解信封」，并发准入、关闭排空与回调 ID 由 internal/hostabi 负责。

import (
	"encoding/json"
	"fmt"
	"unsafe"

	"freetier2api-plugin/internal/hostabi"
)

/*
#include "bridge.h"
*/
import "C"

// nativeHostCallScoped 是宿主回调的真实实现（由 hostabi.Configure 注入）。
func nativeHostCallScoped(callbackID string, method string, payload any) (json.RawMessage, error) {
	rawPayload, errMarshal := hostabi.MarshalPayload(callbackID, payload)
	if errMarshal != nil {
		return nil, fmt.Errorf("%s: %w", method, errMarshal)
	}
	cMethod := C.CString(method)
	defer C.free(unsafe.Pointer(cMethod))

	var response C.cliproxy_buffer
	var requestPtr *C.uint8_t
	if len(rawPayload) > 0 {
		cPayload := C.CBytes(rawPayload)
		if cPayload == nil {
			return nil, fmt.Errorf("%s: allocate host callback payload", method)
		}
		defer C.free(cPayload)
		requestPtr = (*C.uint8_t)(cPayload)
	}

	callCode := C.call_host_api(cMethod, requestPtr, C.size_t(len(rawPayload)), &response)
	var rawResponse []byte
	if response.ptr != nil && response.len > 0 {
		rawResponse = C.GoBytes(response.ptr, C.int(response.len))
	}
	if response.ptr != nil {
		C.free_host_buffer(response.ptr, response.len)
	}
	if len(rawResponse) == 0 {
		return nil, fmt.Errorf("%s: host callback returned no response, code=%d", method, int(callCode))
	}
	result, errDecode := hostabi.DecodeEnvelope(rawResponse)
	if errDecode != nil {
		return nil, fmt.Errorf("%s: %w", method, errDecode)
	}
	if callCode != 0 {
		return nil, fmt.Errorf("%s: host callback returned code=%d", method, int(callCode))
	}
	return result, nil
}

// init 注入 CGO 实现：之后 root 与 internal 包统一走 hostabi.Call。
func init() {
	hostabi.Configure(nativeHostCallScoped)
}
