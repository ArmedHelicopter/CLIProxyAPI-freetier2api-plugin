# AutoClaw 与小浣熊（Raccoon）骨架实现说明

> 状态：**骨架（skeleton）** —— 结构性能力已就绪，上游协议细节待补充。
> 关联 Issue：[#24 autuclaw国内/国际、小浣熊支持](https://github.com/jiandanc/CLIProxyAPI-freetier2api-plugin/issues/24)

## 1. 为什么是骨架而不是完整实现

Issue #24 只有一句话：「能不能加个autoclaw国内/国际、小浣熊呢」，没有附带：

- 上游 API 域名（国内版 / 国际版分别是什么）；
- 鉴权形态（是 Bearer API key、自定义头，还是 OAuth）；
- chat-completions 是否为 OpenAI 兼容协议、真实路径；
- 模型清单接口与响应形态；
- 额度 / 签到等扩展接口是否存在。

本仓库的既有供应商实现（workbuddy、qoder 等）全部基于**实测协议**
（多数参考对应的开源 2API 项目）。没有实测依据就写死域名与请求体，
等于臆造端点——用户配置后会得到必然失败的模型与账号。因此本 PR
按仓库纪律只落实**结构**，把协议细节的回填点全部显式标出。

## 2. 已实现的内容

### 2.1 AutoClaw（国内版 / 国际版双实例）

| 项 | 值 |
| --- | --- |
| 供应商 ID | `autoclawcn` / `autoclawglobal` |
| 展示名 | AutoClaw 国内版 / AutoClaw 国际版 |
| 区域 | `cn` / `global` |
| 认证方式 | API key（Bearer） |
| 凭证文件名前缀 | `autoclawcn-*.json` / `autoclawglobal-*.json`（或任意含 `autoclaw` 段的文件名） |
| 配置键 | `autoclawcn_base_url` / `autoclawglobal_base_url` |

- `internal/vendors/autoclaw`：协议 × 区域的双实例适配器，实例在构造期固定区域；
- 凭证解析：`api_key` / `apikey` / `autoclaw_key` / `token` 等字段名均接受；
- 归属判定**偏严**：只认文件名约定、`vendor: autoclaw*` 声明或
  `autoclaw_key` 专有字段；通用的 `api_key` / `token` 不作为判据
  （避免误吞 OpenCode ZEN 等别家凭证）；
- 对话与模型清单：OpenAI 兼容路径（`/v1/chat/completions`、`/v1/models`），
  支持流式透传；
- 能力边界（刻意的）：无登录流程、无额度接口、无签到——页面会如实显示
  「不支持」，而不是假装支持然后失败。

### 2.2 小浣熊（Raccoon，单实例）

| 项 | 值 |
| --- | --- |
| 供应商 ID | `raccoon` |
| 展示名 | 小浣熊 |
| 区域 | 无（单部署） |
| 认证方式 | API key（Bearer） |
| 凭证文件名前缀 | `raccoon-*.json`（含拼音 `xiaohuanxiong` 段亦可） |
| 配置键 | `raccoon_base_url` |

结构与 AutoClaw 相同；若后续确认有国内/国际双部署，按 workbuddy /
autoclaw 的双实例模式扩展即可。

### 2.3 注册与兼容性

- `internal/app/vendors.go`：组合根新增 `autoclaw.Instances` 与
  `raccoon.Instances`（autoclaw 双实例排在最前，与字母序一致）；
- `tests/integration/vendor_compat_test.go`：对外契约快照更新为
  16 个实例，并锁定新供应商的能力集合（apikey 型、无 quota）；
- `tests/integration/quota_capability_generic_test.go`：no-quota 快照
  加入 `autoclawcn` / `autoclawglobal` / `raccoon`；
- `tests/integration/auth_test.go`：跨供应商归属判别用例加入新前缀
  与「通用字段名不误吞」回归。

## 3. 默认端点为空：运行期行为

两个供应商的默认基地址都是**空串**（见 `protocol.go` 的
`DefaultBaseURL*`）。这意味着在管理员配置 `*_base_url` 之前：

- 模型探测与对话请求会返回明确的「upstream base URL is not configured」
  错误，而不是把猜测的域名当成可用端点；
- 控制台仍会显示供应商与账号（凭证解析、归属、脱敏展示均已生效）。

这遵循本插件一贯的原则：**宁可明确失败，不可静默打错地方**。

## 4. 回填上游信息的最小清单（TODO(upstream)）

拿到以下任一信息后即可把骨架升级为完整实现（改动集中在各包的
`protocol.go`，必要时加 `payload.go`）：

1. **域名**：国内版 / 国际版的 API 基地址 → 回填 `DefaultBaseURLCN` /
   `DefaultBaseURLGlobal` / `raccoon.DefaultBaseURL`；
2. **鉴权头**：确认是否为标准 `Authorization: Bearer <key>`，有无专有
   客户端标识头（UA、版本、设备指纹等） → 调整 `ApplyChatHeaders`；
3. **路径与请求体**：chat-completions 真实路径；请求体是否需要额外字段
   （session、user、提示词注入等） → 调整 `ChatPath` / `prepareBody`；
4. **模型清单**：路径与响应形态（本实现按 OpenAI `{"data":[{"id":...}]}`
   解析并兼容裸数组） → 调整 `ModelsPath` / `parseModels`；
5. **错误码**：业务错误码到 `core.FailureKind` 的映射（额度耗尽、限流、
   内容风控等） → 细化 `Classify`；
6. **扩展能力**：是否存在额度查询、签到、成长任务接口 → 按需实现
   `QuotaProvider` / `CheckinProvider` 等可选接口。

信息可以直接回复在 Issue #24，或以 PR 形式补充到本文件与对应包的
注释里。

## 5. 验证方式

```bash
go build ./...
go vet ./...
go test ./internal/vendors/autoclaw/... ./internal/vendors/raccoon/...
go test ./tests/integration/ -run 'TestVendorRegistryCompatibility|TestVendorCapabilities|TestSupportsQuotaIsGenericAcrossVendors|TestAuthParseVendorOwnershipDiscrimination'
```

沙箱内无法绑定 TCP 端口的测试（trae/codearts/qoder/zcode 的回调监听与
httptest 用例）在本机基线上同样失败，与本次改动无关。

