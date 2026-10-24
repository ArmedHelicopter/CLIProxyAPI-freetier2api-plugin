# AutoClaw 与小浣熊（Raccoon）实现说明

> 状态：**完整实现**（协议依据两个开源逆向项目，见下）。
> 关联 Issue：[#24 autuclaw国内/国际、小浣熊支持](https://github.com/jiandanc/CLIProxyAPI-freetier2api-plugin/issues/24)

## 1. 协议来源

| 供应商 | 参考项目 | 依据 |
| --- | --- | --- |
| AutoClaw（国内/国际） | [Zhengyuuuui/autoclaw2api](https://github.com/Zhengyuuuui/autoclaw2api) | `docs/reverse-notes.md`（2026-09 Web bundle + 桌面端 1.18.5 静态逆向与抓包） |
| 小浣熊（商汤 Raccoon） | [misakano7545/xiaohuanxiong2api-panel](https://github.com/misakano7545/xiaohuanxiong2api-panel) | `internal/upstream`（v1.1.0 桌面客户端抓包） |

## 2. AutoClaw（`autoclawcn` / `autoclawglobal`）

| 项 | 值 |
| --- | --- |
| 供应商 ID | `autoclawcn` / `autoclawglobal` |
| 展示名 | AutoClaw 国内版 / AutoClaw 国际版 |
| 上游基址 | cn: `https://autoglm-acceleration-api.zhipuai.cn`；global: `https://autoglm-api.autoglm.ai` |
| 认证方式 | 用户粘贴 `accessToken` / `refreshToken` 凭证 JSON（登录是手机验证码 / 桌面端 OAuth，插件侧无法托管验证码交互，无登录入口） |
| 凭证文件名前缀 | `autoclawcn-*.json` / `autoclawglobal-*.json` |
| 配置键 | `autoclawcn_base_url` / `autoclawglobal_base_url` |

实现要点：

- **签名**：全部 userapi 请求带 `X-Auth-Appid: 100003` + `X-Auth-TimeStamp` +
  `X-Auth-Sign`（`MD5("100003&<unix秒>&<secret>")`，secret 与客户端版本常量
  逆向自 Web bundle，见 `protocol.go`）；
- **对话**：`POST {region基址}/autoclaw-proxy/proxy/autoclaw/v1/chat/completions`，
  模型经 `X-Request-Model` 头与 body `model` 双通道声明（autoclaw2api 的
  proxy 通道；沙箱 relay 通道依赖常驻 WebSocket 会话，与插件按请求出站的
  模型不符，未实现，凭证字段已预留）；
- **模型清单**：global 实时拉 `autoclaw-model-config`；cn 用静态表
  （上游无对应的可直拉目录，模型表来自 autoclaw2api 快照）；
- **续期**：`POST /userapi/v1/refresh`（body 带 `source_id` / `device_id` /
  `refresh_token`；上游响应无 `expires_in`，按 28 天保守记录）；
- **额度**：`GET /agent-assetmgr/api/v2/wallets?biz_app_id=autoclaw` 的
  `total_balance`；
- **签到**：`POST .../autoclaw-task-complete` body `{"task_id":"daily_signin"}`，
  服务端幂等（`already_completed=true` 表示今日已签）；
- **错误分类**：业务码 `410000` = 会话失效；余额/额度文案 → hard_credit；
  429 → soft_rate。

## 3. 小浣熊（`raccoon`）

| 项 | 值 |
| --- | --- |
| 供应商 ID | `raccoon` |
| 展示名 | 小浣熊 |
| 上游基址 | `https://xiaohuanxiong.com`（四域：`/api/web/llm/v2`、`/api/web/auth/v1`、`/api/web/points/v1`、主站 origin） |
| 认证方式 | **微信扫码登录**（控制台「添加账号」入口）+ 授权码回调粘贴兜底 + 手工粘贴凭证 JSON |
| 凭证文件名前缀 | `raccoon-*.json`（含拼音 `xiaohuanxiong` 段亦可） |
| 配置键 | `raccoon_llm_base_url` / `raccoon_auth_base_url` / `raccoon_points_base_url` / `raccoon_origin` |

实现要点：

- **扫码登录**：`qrcode_code` 由客户端本地生成（32 位 hex），二维码编的是公开
  网页 `{origin}/login/mp?code=…&appname=商汤小浣熊官网`；轮询
  `POST {auth_base}/login_with_qrcode_code` 取票——无需回调地址，最适合
  远程部署的插件形态。另有 `office-raccoon://auth/callback` 授权码换票兜底
  （业务码 `200035` = 授权码失效）；
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
- 出站带 v1.1.0 桌面客户端识别头（`x-client-name` 等）。

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

