package admin

import (
	"encoding/json"
	"net/http"
	"testing"

	"freetier2api-plugin/cpasdk/pluginapi"
)

func TestDecodeJSONBodyRejectsMalformedJSON(t *testing.T) {
	var payload struct {
		Name string `json:"name"`
	}
	if err := decodeJSONBody([]byte(`{"name":`), &payload); err == nil {
		t.Fatal("decodeJSONBody accepted malformed JSON")
	}
	if err := decodeJSONBody(nil, &payload); err != nil {
		t.Fatalf("empty body should be accepted: %v", err)
	}
}

func TestBadJSONRequestResponseIs400(t *testing.T) {
	response := BadJSONRequestResponse()
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", response.StatusCode, http.StatusBadRequest)
	}
	var payload map[string]any
	if err := json.Unmarshal(response.Body, &payload); err != nil {
		t.Fatalf("body is not JSON: %v", err)
	}
	if payload["ok"] != false || payload["error"] != "请求体解析失败，请检查 JSON 格式" {
		t.Fatalf("body = %s", response.Body)
	}
}

func TestModelHandlersRejectMalformedJSON(t *testing.T) {
	service := newService(t)
	request := pluginapi.ManagementRequest{Method: http.MethodPost, Body: []byte(`{"models":`)}
	for name, handler := range map[string]func(pluginapi.ManagementRequest) pluginapi.ManagementResponse{
		"toggle": service.modelToggleHandler,
		"alias":  service.modelAliasHandler,
	} {
		response := handler(request)
		if response.StatusCode != http.StatusBadRequest {
			t.Errorf("%s status = %d, want %d", name, response.StatusCode, http.StatusBadRequest)
		}
	}
}
