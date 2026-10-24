# AutoClaw 与小浣熊（Raccoon）实现说明

> 状态：**完整实现**（协议依据两个开源逆向项目，见下）。
> 关联 Issue：[#24 autuclaw国内/国际、小浣熊支持](https://github.com/jiandanc/CLIProxyAPI-freetier2api-plugin/issues/24)

## 1. 协议来源

| 供应商 | 参考项目 | 依据 |
| --- | --- | --- |
| AutoClaw（国内/国际） | [aimod-cc/Agent2API](https://github.com/aimod-cc/Agent2API) | `providers/autoclaw`（桌面端 1.17.8 头集合、proxy 通道、签名与签到链路实测） |
| 小浣熊（商汤 Raccoon） | [aimod-cc/Agent2API](https://github.com/aimod-cc/Agent2API) | `providers/raccoon`（网页授权登录、目录、积分与签到口径） |

## 2. AutoClaw（`autoclawcn` / `autoclawglobal`）

| 项 | 值 |
| --- | --- |
| 供应商 ID | `autoclawcn` / `autoclawglobal` |
| 展示名 | AutoClaw 国内版 / AutoClaw 国际版 |
| 上游基址 | cn: `https://autoglm-acceleration-api.zhipuai.cn`；global: `https://autoglm-api.autoglm.ai` |
| 认证方式 | **国内版：手机号 + 短信验证码登录**（控制台表单）；**国际版：Google OAuth 网页登录**；另支持粘贴 `accessToken`/`refreshToken` JSON |
| 凭证文件名前缀 | `autoclawcn-*.json` / `autoclawglobal-*.json` |
| 配置键 | `autoclawcn_base_url` / `autoclawglobal_base_url` |

实现要点：

- **认证头**：对话走 `X-Authorization: Bearer <token>`（源实现口径，发 `Authorization` 反而 401）；brand 头集合 `X-Product: autoclaw` / `X-Client-Type: pc` / `X-Tm: mac` / `X-Version: 1.17.8` / `X-Lang` / `X-Channel: official` / `x_trace_id`（**不发** `X-Harness-Type`，带上会被区别对待）； `X-Auth-Appid: 100003` + `X-Auth-TimeStamp` +
  `X-Auth-Sign`（`MD5("100003&<unix秒>&<secret>")`，secret 与客户端版本常量
  逆向自 Web bundle，见 `protocol.go`）；
- **对话**：`POST {region基址}/autoclaw-proxy/proxy/autoclaw/chat/completions`，
  模型经 `X-Request-Model` 头与 body `model` 双通道声明（Agent2API 的
  proxy 通道；沙箱 relay 通道依赖常驻 WebSocket 会话，与插件按请求出站的
  模型不符，未实现，凭证字段已预留）；
- **模型清单**：global 实时拉 `autoclaw-model-config`；cn 用静态表
  （上游无对应的可直拉目录，模型表来自 Agent2API 快照）；
- **续期**：`POST /userapi/v1/refresh`（body 带 `source_id` / `device_id` /
  `refresh_token`；上游响应无 `expires_in`，按 28 天保守记录；签名校验失败降级 `agent-refresh`）；
- **额度**：`GET /agent-assetmgr/api/v2/wallets?biz_app_id=autoclaw` 的
  `total_balance`；
- **签到**：`POST .../autoclaw-task-complete` body `{"task_id":"daily_signin"}`，
  服务端幂等（`already_completed=true` 表示今日已签）；
- **登录**：国内版 `POST /userapi/v1/agent-send-code` + `agent-login/`（验证码必须是 JSON 数字，字符串会得到与验证码错误无关的 400001；两次调用用同一 device_id）；国际版 Google OAuth（`oauth-url` → 浏览器登录 → 回调链接粘贴 → `oauth-login` 换码，第③跳不需要风控验证码）；Zai 因回调白名单只认官方客户端四个端口且第②步强制风控验证码，暂不支持；
- **错误分类**：业务码 `410000` = 会话失效；余额/额度文案 → hard_credit；
  429 → soft_rate。

## 3. 小浣熊（`raccoon`）

| 项 | 值 |
| --- | --- |
| 供应商 ID | `raccoon` |
| 展示名 | 小浣熊 |
| 上游基址 | `https://xiaohuanxiong.com`（四域：`/api/web/llm/v2`、`/api/web/auth/v1`、`/api/web/points/v1`、主站 origin） |
| 认证方式 | **网页授权登录**（官方授权页 `/code/authorize`，控制台「添加账号」入口）+ 回调链接粘贴 + 手工粘贴凭证 JSON |
| 凭证文件名前缀 | `raccoon-*.json`（含拼音 `xiaohuanxiong` 段亦可） |
| 配置键 | `raccoon_llm_base_url` / `raccoon_auth_base_url` / `raccoon_points_base_url` / `raccoon_origin` |

实现要点：

- **网页授权登录**：`{origin}/code/authorize?login_source=desktop&appname=办公小浣熊客户端&state=<state>`（Agent2API oauth.rs 口径）；登录成功后官方页跳 `office-raccoon://auth/callback?code=…&state=…`，用户把整条链接粘回控制台，`POST {auth_base}/login_with_authorization_code` 换票（业务码 `200035` = 授权码失效）。state 由插件生成、一次性消费、逐字比对；
- **对话**：`POST {llm_base}/chat/completions`，上游即 OpenAI 形状，原样转发；
- **模型目录**：`GET {llm_base}/model_catalog`（`categories[].models` 平铺、
  同名首见者胜、`context_window`/`max_tokens` 数字/字符串/params 嵌套三形态
  兼容、`visible=false` 过滤）；目录失败回落静态单条 `raccoon-8c4485`；
- **续期**：`POST {auth_base}/refresh`，refresh_token **可能轮换**，新票写回；
- **额度**：`GET {points_base}/balance` 的 `available_points`；
- **签到**：每日发放由服务端 00:00 自动入账；登录领取走
  `POST {origin}/api/web/desktop/v1/login/points/grant`（幂等，已领回
  `granted:false`）；
- **错误分类**：401/403 → 会话失效；429 → 限流；积分/额度文案（含业务码
  `14018`）→ hard_credit；`insufficient` 单独出现不判额度（避免
  "context window is insufficient" 误伤）；
- 出站带桌面客户端识别头。早期参考项目里的微信扫码通道（`/login/mp` 承载页）已随上游页面下线而不可用（实测 404，2026-10-24），故未采用。

## 4. 归属判定纪律

两家都遵循仓库「凭证归属必须偏严」的同一纪律（误吞别家凭证 = 宿主静默
覆盖对方账号；漏认 = 加载失败，代价小得多）：

- AutoClaw：文件名约定 / `vendor: autoclaw*` 声明 / `sandboxEndpoint`
  专有字段；通用 `accessToken` 对、单独 `deviceId` 不作判据；
- 小浣熊：文件名约定 / `vendor: raccoon` / snake_case
  `access_token`+`refresh_token`+`uid` 三件套；驼峰 token 对、单独
  `token` 不作判据。

## 5. 配置项

```yaml
freetier2api:
  autoclawcn_base_url: ""      # 覆盖国内版上游基址
  autoclawglobal_base_url: ""  # 覆盖国际版上游基址
  raccoon_llm_base_url: ""     # 覆盖 LLM 网关基址
  raccoon_auth_base_url: ""    # 覆盖鉴权基址
  raccoon_points_base_url: ""  # 覆盖积分基址
  raccoon_origin: ""           # 覆盖主站 origin
```

全部留空即用内置默认值（与参考项目一致的实测域名）。

## 6. 验证方式

```bash
go build ./...
go vet ./...
go test ./internal/vendors/autoclaw/... ./internal/vendors/raccoon/...
go test ./tests/integration/ -run 'TestVendorRegistryCompatibility|TestVendorCapabilities|TestSupportsQuotaIsGenericAcrossVendors|TestAuthParseVendorOwnershipDiscrimination'
```

