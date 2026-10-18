# cpasdk

本目录是 CLIProxyAPI 宿主 ABI/JSON 契约的 **vendored 副本**，不是第三方依赖：

- `pluginabi/types.go` 对齐宿主 `sdk/pluginabi/types.go`；
- `pluginapi/types.go` 对齐宿主 `sdk/pluginapi/types.go`。

同步方式：从宿主对应版本复制这两个文件，保持排版一致；`gofmt` 检查刻意排除本目录。
宿主升级 ABI 时先更新这里，再调整 `internal/app` 的 RPC 适配层。
