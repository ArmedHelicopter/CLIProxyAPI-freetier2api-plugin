package cline

import (
	"testing"

	"freetier2api-plugin/internal/core"
)

func TestConfigureBaseURLPerInstance(t *testing.T) {
	v := Instances(core.Env{})[0].(*adapter)
	v.Configure(core.Settings{}, map[string]string{"cline_base_url": "http://cline.test"})
	if v.baseURL != "http://cline.test" {
		t.Fatalf("baseURL = %q", v.baseURL)
	}
}
