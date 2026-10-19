# freetier2api-plugin

把**多个免费额度供应商**接入 **CLIProxyAPI**（下称 CPA）的原生动态库插件。

插件通过宿主的 `cliproxy_plugin_init` ABI 加载，一次性声明五类能力，
宿主的路由、鉴权、调度、日志与代理全部复用——**不需要再单独跑一个网关服务**：

| 能力 | 作用 |
| --- | --- |
| `auth_provider` | 识别并加载各家凭证，支持设备授权登录与定时刷新 |
| `model_provider` | 注册模型（裸名，跨供应商同名合并），清单从上游动态探测 |
| `executor` | 转发 chat-completions（含流式）；Claude / Codex 客户端由宿主翻译 |
| `quota_provider` | 查询账号额度，供管理端展示 |
| `management` | 自有管理接口 + 内嵌控制台页（账号、模型、任务、设置、日志） |

## 支持的供应商

| 供应商 ID | 展示名 | 凭证文件名前缀 |
| --- | --- | --- |
| `workbuddycn` | WorkBuddy 国内版 | `workbuddycn-*.json` |
| `workbuddyglobal` | WorkBuddy 国际版 | `workbuddyglobal-*.json` |
| `qodercn` | Qoder 国内版 | `qodercn-*.json` |
| `qoderglobal` | Qoder 国际版 | `qoderglobal-*.json` |
| `opencodezen` | OpenCode ZEN | `opencodezen-*.json` |
| `cline` | Cline | `cline-*.json` |
| `zcode` | ZCode | `zcode-*.json` |
| `traesolo` | TRAE SOLO | `traesolo-*.json` |
| `traecn` | Trae 国内版 | `traecn-*.json` |
| `traeglobal` | Trae 国际版 | `traeglobal-*.json` |
| `codearts` | CodeArts | `codearts-*.json` |
| `minimaxcodecn` | MiniMax Code 国内版 | `minimaxcodecn-*.json` |
| `minimaxcodeglobal` | MiniMax Code 国际版 | `minimaxcodeglobal-*.json` |

## 架构：按供应商插桩

宿主 ABI 有一个硬性约束：**一个插件进程只能声明一个 provider key**
（`auth.identifier` 返回单个字符串、注册期调一次永久缓存，且与 auth 文件的
`type` 字段严格相等才认领该文件）。因此本插件用统一的 `providerKey = "freetier"`，
**供应商的区分落在凭证文件名前缀与文件内的 `vendor` 字段上**。

依赖方向始终单向：

```
package main / cgo        C ABI 入口
  └─ internal/app         组合根与 ABI 适配（生命周期、RPC 分发、五类能力）
       ├─ internal/admin    管理接口：HTTP 路由/响应 + 全局配置 + 模型目录 + 控制台页
       ├─ internal/tasks    供应商无关的任务目录、排程、队列与执行引擎
       ├─ internal/auth     账号枚举/去重/批量执行/额度格式化 + 登录会话登记
       └─ internal/vendors/* 各供应商协议与适配层
            └─ internal/core 供应商无关契约（Vendor、能力接口、注册表、凭证、错误分类）
```

**供应商包只允许依赖 `core`（契约）+ `hostbridge`（宿主通信）+ `logger`（日志）**，
多一个都不行。这条 allowlist 由 `tests/architecture_test.go` 静态守住，它挡住
vendor 反向依赖 `auth`/`state`/`tasks`/`admin` 的所有可能——例如某个供应商为了
发一次宿主 RPC 或换一个随机 ID 就去 import 应用层包。

其余分层：`core` 只依赖标准库；`state` 依赖 `core`/`logger`；`hostbridge` 依赖
`core`/`logger`；`auth` 依赖 `core`/`hostbridge`；`tasks` 依赖 `core`/`auth`/
`state`/`logger`；`admin` 依赖 `core`/`auth`/`tasks`/`state`/`hostbridge`；
`app` 在最上，且只有 `internal/app/vendors.go` 可以 import 具体供应商。
新增内部包必须同步登记到 `layerRules` / `internalPackages`，否则测试直接失败。

```
package main                        C ABI 入口（main.go / cgo.go）
internal/app                        组合根与 ABI 适配
  ├─ main.go                        插件身份、RPC 方法分发、注册响应
  ├─ host.go                        core.Host 端口、生命周期、供应商配置下发
  ├─ config.go                      全局配置适配 + state.json/机器盐
  ├─ authprovider.go                auth provider（解析/登录/刷新/凭证写回）
  ├─ models.go                      model provider（注册、隔离、别名、禁用）
  ├─ executor.go                    executor + quota provider
  └─ vendors.go                     BuiltinFactories()：唯一的供应商清单
internal/core                       供应商无关骨架：Vendor 接口 + 可选能力接口
                                    （登录/续期/额度/签到/任务/配置/存储合并…）、
                                    注册表、凭证、模型 ID 协议、任务契约、
                                    上游错误分类（FailureKind/Failure）、
                                    凭证 JSON 合并骨架（MergeCredentialJSON）与共享 helper
internal/admin                       管理接口（传输层 + 配置 + 模型目录 + 控制台页）
  ├─ admin.go                       服务构造、路由声明与总分发
  ├─ config.go                      全局配置解析/校验（供应商专属键保留在 Raw）
  ├─ catalog.go                     模型目录：静态缓存、别名、禁用、同名合并、刷新
  ├─ console.go / console.html      内嵌控制台页面（go:embed）
  └─ accounts/models/quota/settings/tasks/vendors/keepalive.go   各管理接口处理
internal/auth                        账号枚举/去重/批量执行 + 额度格式化
                                    + 登录会话登记（SessionRegistry）
internal/tasks                       任务目录、排程、队列、执行引擎、任务 API 与保活编排
internal/state                       state.json 的类型化读写与并发快照
internal/hostbridge                 与宿主通信的唯一入口
  ├─ rpc.go / auth.go / management.go  宿主回调、凭证枚举、管理请求上下文、并发准入
  ├─ http.go                          出站 HTTP 桥（宿主代理/日志）：Client / StreamClient
  └─ sse.go                           原始 SSE 转发
internal/logger                      内存日志环与文件落盘
tests/architecture_test.go         分层与测试布局静态守卫
tests/integration                  跨包集成测试（假宿主）
tests/e2e                          完整 ABI 端到端
tests/live                         实况探针（-tags live）
tests/testhost                     共享假宿主

internal/vendors/<name>            协议层 + 适配层同包，文件名是**封闭白名单**：
  ├─ adapter.go       [必需] core.Vendor 实现（嵌入 core.Meta）+ Instances/Configure/Match
  │                          + 可选能力方法（薄转发）+ ConfigFields/RuntimeFields
  ├─ chat.go          [必需] HTTP 传输（端点表/请求头/签名/会话基建）
  │                          + 对话（请求体变换/协议转换/SSE 归一化/Chat/ChatStream）
  ├─ credential.go    [必需] 凭证类型/解析/归属判定/存储写回 + 设备指纹与身份头
  ├─ models.go        [必需] 模型目录与探测 + bundledModels() 兜底 + 控制台模型明细
  ├─ login.go         [必需] 账号接入：AuthMode + 登录流程 + 续期
  │                          （纯 API key 供应商只放脱敏等接入辅助）
  ├─ errors.go        [可选] 上游错误分类：Classify(status, body) *core.Failure
  ├─ tasks.go         [可选] 账号运营类能力：额度/签到/套餐领取/任务/每日奖励
  ├─ protocol.go      [可选] 专有协议引擎（qoder：建链、模型列举、排队信号）
  ├─ payload.go       [可选] 请求体加工与指纹清洗（workbuddy）
  ├─ prompt.go        [可选] 文本提示词与工具调用解析（minimaxcode）
  ├─ *_test.go        [按需] 必须与源文件同名；实况测试用 *_live_test.go
  └─ assets/          [按需] go:embed 的内嵌资源（json/md），命名语义化
```

八个供应商包的文件名集合**完全同构**：读任何一家都能立刻在另一家找到对应实现。
可选文件按能力存在，**不需要就整份缺失**（不写空实现）——例如 opencodezen 没有
`tasks.go`；八家现在都有 `errors.go`。约定一条不落：

- 单实例供应商用 `VendorID`/`VendorName` 常量，多实例供应商用
  `VendorIDFor(region)`/`VendorNameFor(region)`；工厂统一是
  `Instances(env core.Env) []core.Vendor`。
- 错误分类统一返回 `*core.Failure`（不再有 `type Error = core.Failure` 这类别名），
  兜底模型清单统一叫 `bundledModels()` / `bundledModelsFor(cfg)`。
- 内嵌资源一律放 `assets/`，`go:embed` 写相对路径。

刻意的差异（都不是漏改）：

- 只有 `qoder` 需要 `protocol.go`、只有 `workbuddy` 需要 `payload.go`、只有
  `minimaxcode` 需要 `prompt.go`。
- `qoder` 原有的 `bridge/`、`cosy/`、`qoderapi/` 三个子包已扁平化进顶层包：
  现在 8 个包都是**单包**，`tests/architecture_test.go` 的 `layerRules` 里
  qoder 只剩一条单包条目。

供应商的可变状态（base URL、运行时配置、提示词、档位表、设备令牌缓存、
登录会话、领取节流窗口、登录回调监听）一律放在 adapter/runtime/SessionStore
实例里，不声明可变包级变量；只读的内嵌 JSON、静态端点表与正则等可作为
包级常量/只读数据。这样同一进程内多个 App 实例互不影响。

Trae 的授权回调监听也随实例：优先 `127.0.0.1:18080`，端口被占用时退回系统
分配的空闲端口，并把**实际**端口写进登录链接（否则回调永远到不了本进程）。

Trae 与 CodeArts 的上游**只认 localhost 回调**。插件与浏览器不在同一台机器时
（远程主机、Docker、WSL），那条回调永远到不了：Trae 的会话会一直停在等待，
CodeArts 更是拿不到上游签发的票据（浏览器回调里的 ticket_id / secret 是华为云
签发的，不是 LoginStart 送进 authorize 的那一对）。因此这两家实现了
core.CallbackSubmitter：控制台明示本机回调地址，用户把浏览器停住的那条整条链接
粘回来，插件就地解析完成登录——同机部署照旧走自动回调，远程部署不再卡住。

### 新增 / 删除供应商

新增：

1. 新建 `internal/vendors/<name>/`，先把五个必需文件补齐
   （`adapter.go`/`chat.go`/`credential.go`/`models.go`/`login.go`），
   再按能力加可选文件（`errors.go`/`tasks.go`/…）；只实现自己支持的可选能力
   接口，不支持的能力**不写空实现、也不建文件**。
2. 在 `internal/app/vendors.go` 的 `BuiltinFactories()` 里加一行
   `{Instances: <name>.Instances}`。
3. 在本文件的供应商表与凭证字段说明里补一行；如有区域/provider key 差异，
   在 vendor 包内用 `Instances(env)` 返回多个实例。
4. 补与源文件同名的测试（至少 `adapter_test.go` 实例配置隔离、
   `credential_test.go` 归属判定）与 `tests/integration/` 下的归属/注册回归。

删除：删除 vendor 目录与 `BuiltinFactories()` 中对应的一行，删除 README 表项；
其余层不需要改动。

### 凭证归属判定

`core.ResolveVendor` 按证据由强到弱判定：

1. **凭证内的 `vendor` 字段** —— 插件自己写的，最可靠；
2. **文件名前缀** —— 用户可读，且登录落盘时就按供应商命名；
3. **各供应商自己的 `Match`** —— 内容嗅探兜底（兼容手写/改名凭证）。

刻意不用「第一个认领的就收下」：供应商之间的结构有重叠（都可能有 token 字段），
靠遍历顺序决定归属会让结果随注册顺序漂移。

**归属判定必须偏严**：宿主会把所有非内建格式的凭证依次喂给每个插件，误吞别家的
会让宿主用本插件的结构覆盖对方账号（且是静默的）。

## 特性

- **供应商隔离**：每个凭证只注册它自己供应商的模型，宿主在选号阶段据此淘汰
  不匹配的凭证——插件不需要自己实现任何调度逻辑。
- **模型清单动态探测**：WorkBuddy 国内版两路（`/v3/config` + 企业目录）、
  国际版三路（v3 的 IDE UA + CLI UA + 企业目录），Qoder 走自己的模型列表接口。
- **额度查询**：各家的额度口径不同（WorkBuddy 是积分余额，Qoder 是套餐用量桶），
  在适配层统一折算成「剩余 / 总额」供页面展示。
- **签到与任务**：WorkBuddy 国内版支持每日签到、连登兑换、抽奖、猫猫旅行、
  夜猫子、活跃上报、token 保活，以及成长任务一键完成（17 个任务）与开学季活动；
  WorkBuddy 国际版支持每日活跃奖励（国际版签到，见下）；
  Qoder 国内版支持每日签到。
- **提示词防御与脱敏**（WorkBuddy）：系统提示词三模式（透传 / 替换 / 追加）
  + 出站请求体指纹脱敏，两层叠加降低上游内容审核拦截率。
- **敏感凭证脱敏与操作管理**：API key 类型账号在列表展示前4位与后4位、中间脱敏为 `***`，避免明文泄露；账号概览最后一列提供「操作」列，支持「删除」按钮直接删除本地凭证文件（带二次确认），宿主基于文件监听自动注销失效账号。
- **零第三方依赖**：`go.mod` 只有 module 与 go 版本两行，构建不需要拉包。

## 测试与验证

测试目录约定：

| 目录 | 内容 |
| --- | --- |
| `internal/**/xxx_test.go` | 单元/白盒测试，与源码同目录 |
| `tests/integration/` | 跨包集成测试（plugin register/models/executor/auth/registry、供应商兼容、App 隔离） |
| `tests/e2e/` | 完整 ABI 端到端 |
| `tests/live/` | 实况探针（`-tags live`，需真实凭证与 `MINIMAX_LIVE=1` 等环境变量） |
| `tests/testhost/` | 共享假宿主 |
| `tests/architecture_test.go` | 分层、import 方向与测试布局的静态守卫 |

`tests/architecture_test.go` 用静态 import 图守住分层，并拒绝：供应商互相 import、
`internal/app/vendors.go` 之外的 app 文件 import 具体供应商、`internal/core` import
任何内部包、`admin`/`tasks`/`auth` 之间的同层依赖，以及 `tests/` 根下出现集成测试文件。
新增内部包必须同步登记到 `layerRules` 与 `internalPackages`，否则测试直接失败——
这样「多了一个包却没定边界」不会静默进入依赖图。

本机没有 Go 工具链时用 Docker：

```bash
./scripts/godocker.sh vet ./...
./scripts/godocker.sh test ./... -count=1 -timeout 300s
./scripts/godocker.sh test -race ./internal/core ./internal/state \
  ./internal/auth ./internal/tasks ./internal/admin -count=1
CGO_ENABLED=1 go build -buildmode=c-shared -o dist/freetier2api.so .
```

`gofmt -l .` 应无输出（`cpasdk/` 按约定豁免）。主要覆盖：

| 位置 | 覆盖主题 |
| --- | --- |
| `tests/integration/plugin_*_test.go` | 注册响应契约、realm 隔离、模型合并/禁用/别名、非流式聚合、流式分帧与错误上报、额度重置 |
| `tests/integration/{auth,identity,vendor_compat}_test.go` | 供应商归属矩阵、身份跨路径稳定、13 个实例的 ID/名称/区域 golden |
| `tests/integration/app_isolation_test.go` | 两个 App 的注册表/状态/登录会话/供应商配置互不可见 |
| `internal/core/core_test.go` | 归属三级判定、注册表幂等约束、模型键协议、错误映射、共享 helper |
| `internal/core/credential_json_test.go` | 凭证 JSON 合并骨架：空/损坏/null/数组退回整体替换，正常路径保留无关字段 |
| `internal/auth/session_test.go` | 会话登记/TTL/轮询节流/归属查询/并发（`-race`） |
| `internal/auth/{pool,batch,quota}_test.go` | 账号枚举去重、批量并发顺序、额度摘要格式化 |
| `internal/tasks/{schedule,engine}_test.go` | 任务目录、排程时间、队列聚合、保活编排与供应商无关的任务契约 |
| `internal/vendors/workbuddy/{errors,payload,credential,chat,tasks}_test.go` | 错误分类顺序、请求体改写、凭证双形态、SSE 聚合、任务与签到编排 |
| `internal/vendors/minimaxcode/errors_test.go` | 上游错误分类：凭证先于余额、流内业务码、错误码提取、分类能走到宿主 |
| `internal/vendors/{trae,zcode}/*_test.go` | 实例隔离：登录会话与回调监听、领取节流窗口互不共享 |
| `internal/hostbridge/http_test.go` | 宿主 HTTP 桥双形态键名兼容、缺状态码显式报错 |
| `internal/admin/admin_test.go` | 管理路由声明、任务路由分发、未知路由 404 |
| `tests/architecture_test.go` | 供应商互不 import、分层边界、内部包白名单、测试布局 |
| `internal/admin/{config,catalog,console}_test.go` | 配置列表解析、快照/损坏回退/并发、别名唯一性、控制台密钥闸门与折叠区 |

实况验证（需真实 CPA 与账号）：

```bash
# 1. 账号是否被识别（应带 region 与余额）
curl -H "Authorization: Bearer $CPA_MANAGEMENT_KEY" \
  http://127.0.0.1:8317/v0/management/plugins/freetier2api/status

# 2. 模型清单是否拉到
curl -X POST -H "Authorization: Bearer $CPA_MANAGEMENT_KEY" \
  http://127.0.0.1:8317/v0/management/plugins/freetier2api/models/refresh

# 3. 对话（非流式与流式）
curl http://127.0.0.1:8317/v1/chat/completions \
  -H "Authorization: Bearer $CPA_API_KEY" -H "Content-Type: application/json" \
  -d '{"model":"cn:glm-5.2","messages":[{"role":"user","content":"hi"}]}'
```

## 构建

需要一个 C 编译器（cgo）与 Go 1.22+。本仓库的 `scripts/build.sh` 经 Docker
构建（本机可没有 Go 工具链），它复用 `scripts/godocker.sh` 的容器参数：

```bash
./scripts/build.sh   # 先跑 go test ./...，再产出 dist/freetier2api.<ext>
```

> **必须用 glibc 镜像构建**：宿主 CPA 镜像是 Debian(glibc)，用 alpine(musl)
> 构建出的 `.so` 会以 `libc.musl-aarch64.so.1: cannot open shared object file`
> 加载失败。`godocker.sh` 已固定用 `golang:1.24`（Debian 版，自带 gcc）。
>
> 交叉编译时 `GOOS`/`GOARCH` 会被透传进容器，例如
> `GOOS=windows GOARCH=amd64 ./scripts/build.sh`。

产物按平台命名：Linux `freetier2api.so`、macOS `freetier2api.dylib`、Windows `freetier2api.dll`。

发行版号由 CI 用 `-ldflags -X main.pluginVersion=<tag>` 注入，本地构建保留默认值。

## 安装

### 通过 CPA 插件商店（推荐）

在 CPA 的 `config.yaml` 里把本仓库的 `registry.json` 加为三方源：

```yaml
plugins:
  enabled: true
  store-sources:
    - "https://raw.githubusercontent.com/jiandanc/CLIProxyAPI-freetier2api-plugin/main/registry.json"
```

重启 CPA → 管理面板 → 插件商店 → 搜 **FreeTier 2API** → 一键安装。

### 手动安装

把产物放进 CPA 的插件目录，**文件名必须是 `freetier2api.<ext>`**
（宿主按 `{pluginID}{ext}` 找动态库）：

```yaml
plugins:
  enabled: true
  dir: "/path/to/cpa/plugins"
  configs:
    freetier2api:
      enabled: true
      enabled_realms: "cn,global"
```

## 添加账号

两种方式，在 CPA 的 auth 目录里产出一个 `<vendor>-<uid>.json` 凭证文件：

1. **控制台页登录**（推荐）：打开
   `http://<CPA>/v0/resource/plugins/freetier2api/console` →
   「添加账号 ▾」→ 从下拉里选供应商 → 在浏览器里完成设备授权。
2. **手写凭证**：放进 CPA 的 auths 目录。

**文件名必须以供应商 ID 开头**（如 `workbuddycn-myaccount.json`、
`qodercn-login-abc.json`），否则插件只能靠内容嗅探归属。

> **Trae / CodeArts 卡在「等待授权中…」怎么办**：这两家的上游只认本机回调地址
> （Trae `http://127.0.0.1:18080/authorize`、CodeArts `http://127.0.0.1/oauth/callback`）。
> 若 CPA 与你的浏览器**不在同一台机器**（远程主机、Docker、WSL），浏览器授权后会停在
> 那个打不开的地址上。展开登录框里的「回调没到插件？粘贴回调链接」，把地址栏里的
> **整条**链接粘进去提交即可，插件在本地解析完成登录；与插件同机时点「打开链接」就行。

也可以在文件里加两个显式字段，这样文件名就不受限制：

```json
{
  "type": "freetier",
  "vendor": "workbuddycn"
}
```

- `type` 必须等于本插件的 provider key（`freetier`），宿主靠它把文件交给本插件；
- `vendor` 是供应商实例标识（见上面的支持表），是归属判定最强的证据。

### WorkBuddy 凭证字段

嵌套形（OAuth 登录产物）：

```json
{
  "auth": {"accessToken": "...", "refreshToken": "...", "expiresAt": 1753600000,
           "domain": "www.codebuddy.cn", "realm": "cn"},
  "account": {"uid": "...", "nickname": "我的账号"},
  "device_token": "..."
}
```

| 字段 | 必填 | 说明 |
| --- | --- | --- |
| `auth.accessToken` | 是 | 出站 Bearer 令牌 |
| `auth.refreshToken` | **强烈建议** | 宿主靠它驱动自动刷新；缺失时账号不会被续期 |
| `auth.expiresAt` | 否 | Unix 秒；缺失时插件首次刷新后自动补上 |
| `auth.domain` | 否 | 出站 `X-Domain`；也用于推断域 |
| `auth.realm` | 否 | `cn` / `global`；缺失时按 `domain` 推断 |
| `account.uid` | 建议 | 设备指纹的派生种子；缺失会导致指纹不稳定 |
| `device_token` | 否 | `X-Device-Token`；桌面端凭证需要 |

扁平形（顶层直接放 `accessToken` / `refreshToken` / `uid` / `domain`）也支持。

### Qoder 凭证字段

```json
{
  "device_token": "dt-...",
  "refresh_token": "drt-...",
  "region": "cn",
  "label": "我的账号",
  "email": "..."
}
```

| 字段 | 必填 | 说明 |
| --- | --- | --- |
| `device_token` | 是 | 设备令牌（`dt-` 前缀）；PAT（`pt-` 前缀）也支持 |
| `refresh_token` | 建议 | 续期用（`drt-` 前缀） |
| `region` | 建议 | `cn` / `global`；缺失时按配置兜底（默认 global） |
| `secret` | 否 | qoder2api 导出格式：整对凭证放在这个 JSON 字符串里 |

### OpenCode ZEN 模型清单与元数据

模型可用性来自上游动态探测（GET https://opencode.ai/zen/v1/models，只返回
模型 ID，免费模型过滤后展示）。上下文窗口、输出上限、推理档位、工具调用等
元数据上游不提供，插件从 https://models.dev 公开目录（provider "opencode"，
MIT）按 ID 合并补全，并按 1 小时缓存；目录不可用时退回通用兜底值
（上下文 1M / 输出 16k / 非推理），不影响模型列表本身。

### OpenCode ZEN 凭证字段

OpenCode ZEN 无登录流程，直接用官网申请的 API key 即可：

```json
{
  "type": "freetier",
  "vendor": "opencodezen",
  "api_key": "sk-...",
  "label": "我的 ZEN Key"
}
```

| 字段 | 必填 | 说明 |
| --- | --- | --- |
| `api_key` | 是 | API Key（`apikey` / `zen_key` 亦兼容） |
| `label` | 否 | 展示名（缺失时自动脱敏显示为 `前4位***后4位`） |

### Cline 凭证字段

通过控制台页的「添加账号 ▾」->「Cline」完成 WorkOS 设备码授权自动落盘，亦可手动填入：

```json
{
  "type": "freetier",
  "vendor": "cline",
  "refreshToken": "...",
  "accessToken": "...",
  "email": "user@example.com"
}
```

| 字段 | 必填 | 说明 |
| --- | --- | --- |
| `refreshToken` | 是 | 长期凭据（WorkOS 设备授权产物，续期核心） |
| `accessToken` | 否 | 出站令牌（缺失时插件首次刷新会自动用 refreshToken 换取） |
| `email` | 建议 | 账号展示邮箱 |

### ZCode 凭证字段

ZCode (Z.AI) 账号由**控制台 OAuth 登录**产生：一次授权同时拿到 Coding Plan
JWT（`data.token`）与兑换出的 API Key，两者一起落盘。

```json
{
  "type": "freetier",
  "vendor": "zcode",
  "auth_mode": "oauth",
  "api_key": "xxx.yyy",
  "jwt_token": "...",
  "email": "user@example.com",
  "user_id": "u-1",
  "label": "user@example.com",
  "profile": {
    "platform": "darwin",
    "arch": "arm64",
    "os_version": "25.5.0",
    "language": "zh-CN",
    "timezone": "Asia/Shanghai",
    "screen": "1512x982",
    "device_mid": "…"
  }
}
```

| 字段 | 必填 | 说明 |
| --- | --- | --- |
| `api_key` | 对话必填 | Z.AI API Key（`api.z.ai` 通道），**免人机验证码** |
| `jwt_token` | 计费必填 | Coding Plan JWT，用于额度查询 |
| `profile` | 建议 | 每账号独立设备档案（一号一台设备，避免多账号被聚成同一台机器） |
| `email` / `user_id` / `label` | 否 | 展示信息，登录时自动从 JWT 解出 |

**两条通道的分工**（重要）：

ZCode 有两条互不相同的上游通道，本插件按能力把它们分开使用：

| 通道 | 端点 | 鉴权 | 验证码 | 用途 |
| --- | --- | --- | --- | --- |
| API Key | `api.z.ai` | `x-api-key` | **不需要** | **对话**（唯一出站路径） |
| Coding Plan | `zcode.z.ai` | `Bearer JWT` | 额度查询不需要，领取需要 | 额度查询、套餐领取 |

Coding Plan 通道的**活动套餐领取**（`billing/claim`）被上游强制要求
`X-Aliyun-Captcha-Verify-Param`，而该验证码是阿里云**无痕**验证（静默采集
浏览器指纹后由服务端判定，没有图片/滑块可作答），无法用视觉模型代答。

本插件不内置浏览器求解器（参考项目的免浏览器求解有 4500 行 Node），改为把
验证码**渲染在控制台页面里**：读取公开端点 `client/configs` 下发的
`sceneId/prefix/region` → 页面加载阿里云 SDK 并挂出控件 → 用户通过后插件只做
转发。因此：

- **对话一律走 API Key 通道**（与验证码完全无关）；
- **套餐领取是全手动**：控制台账号行点「套餐领取」→ 查看可领套餐与额度明细 →
  完成人机验证 → 确认领取。已领完时按钮置灰显示「已领取」；
- **ZCode 不提供签到**（其「签到」实质就是领取活动套餐），控制台按
  「无签到活动」展示，与 Cline / OpenCode ZEN 一致。

领取按钮的状态搭**额度查询**的便车：额度刷新本来就打 `billing/current`，
顺手看一眼 `billing/preview` 就能回答「有没有可领的」，因此没有额外的状态
轮询端点；控制台每次进页面都会刷一次额度，状态随之保持新鲜。

> 阿里云会对验证码令牌做发起端 IP 校验。验证码在你的浏览器里解、领取请求从
> CPA 主机发出，二者出口一致时（自建场景通常如此）可正常工作；跨网络可能出现
> 3007，此时弹窗会提示原因，也可展开「粘贴验证码参数」在同网络下自行提交。

配置项：

| 键 | 类型 | 默认 | 说明 |
| --- | --- | --- | --- |
| `zcode_claim_min_interval` | string | `15s` | 两次领取之间的最小间隔（Go 时长；`0` 表示不节流）。同一出口连打会触发上游 1005/3012 |

直接手填 `api_key`（智谱格式 `xxx.yyy`）同样可用，此时只能对话、没有额度信息。

### TRAE SOLO 凭证字段

TRAE SOLO 账号凭证，支持顶层扁平字段或嵌套 `{auth: {...}, account: {...}}` 结构：

```json
{
  "type": "freetier",
  "vendor": "traesolo",
  "access_token": "...",
  "refresh_token": "...",
  "uid": "123456",
  "nickname": "我的 SOLO 账号"
}
```

| 字段 | 必填 | 说明 |
| --- | --- | --- |
| `access_token` | 是 | 出站 JWT 令牌（携带 `Cloud-IDE-JWT` 头） |
| `refresh_token` | 建议 | `ExchangeToken` 续期令牌，支持轮换自动落盘 |
| `uid` | 建议 | 用户标识（用于派生稳定签到设备 ID 与指纹） |

### Trae 国内版 / 国际版凭证字段

Trae 客户端国内版与国际版共用凭证规范，通过 `region` 或文件名区分：

```json
{
  "type": "freetier",
  "vendor": "traecn",
  "region": "cn",
  "access_token": "...",
  "refresh_token": "...",
  "uid": "123456"
}
```

| 字段 | 必填 | 说明 |
| --- | --- | --- |
| `vendor` | 建议 | `traecn` 或 `traeglobal` |
| `region` | 建议 | `cn`（国内版）或 `global`/`sg`（国际版） |
| `access_token` | 是 | 出站 Bearer/JWT 令牌 |
| `refresh_token` | 建议 | 换取新令牌的刷新凭证 |

### CodeArts 凭证字段

华为云 CodeArts Agent 凭证，包含临时 AK/SK 与 DPoP 私钥材料：

```json
{
  "type": "freetier",
  "vendor": "codearts",
  "access_key_id": "...",
  "secret_access_key": "...",
  "security_token": "...",
  "refresh_token": "...",
  "dpop_private_key": {
    "kty": "EC",
    "crv": "P-256",
    "x": "...",
    "y": "...",
    "d": "..."
  }
}
```

| 字段 | 必填 | 说明 |
| --- | --- | --- |
| `access_key_id` | 是 | 华为云 STS 临时 Access Key |
| `secret_access_key` | 是 | 华为云 STS 临时 Secret Key（用于请求签名） |
| `security_token` | 是 | 华为云 STS Security Token |
| `refresh_token` | 建议 | 续期令牌，结合 `dpop_private_key` 自动轮换 |
| `dpop_private_key`| 建议 | 签发 refresh_token 时绑定的 P-256 ES256 私钥 JWK |

## 客户端接入

```bash
curl http://127.0.0.1:8317/v1/chat/completions \
  -H "Authorization: Bearer <CPA API KEY>" -H "Content-Type: application/json" \
  -d '{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}],"stream":true}'
```

模型用**裸名**（不带供应商前缀）。跨供应商的同名模型由宿主按
`strings.EqualFold` 合并为一个条目，宿主再从各家凭证里选号。

> **注意**：同名模型的选号可能花到另一家供应商的额度。例如 `glm-5.3` 同时存在于
> WorkBuddy 与 Qoder，在宿主眼里是同一个模型。若需要精确指定，用
> `vendor:model` 形式（如 `workbuddycn:glm-5.2`）；若想反向操作——让多个供应商
> 共用一个名字自动调度，见下面的「模型别名与跨供应商路由」。

### 模型别名与跨供应商路由

在控制台页「模型」区块，**点击某行的「别名」单元格**即可编辑：回车或点开别处保存，
Esc 取消；清空保存即清除别名。每个供应商的每个模型各自独立设置。别名就是该模型
注册给宿主的对外模型 ID，客户端以别名调用，出站前插件再还原成上游的官方 ID。

这带来一个关键能力：**把不同供应商的不同模型设成同一个别名**。宿主会把这个
别名当作一个模型，从这几家凭证里选号——任意一家可用即可应答，天然跨供应商
故障转移。例如给 `workbuddycn:glm-5.2` 与 `qodercn:gmodel` 都设别名 `glm-max`，
客户端只请求 `glm-max`，网关会在两家之间自动调度。

- 作用是**供应商内的单个模型 ID**：同名的官方 ID 出现在别的供应商时互不影响，
  各家可各自起别名。
- 别名在**同一供应商内唯一**：把某别名换绑到新模型时，该供应商下占用同名别名
  的旧模型会恢复官方 ID。
- 别名留空即清除，模型恢复用官方 ID 对外。
- 别名**即时生效**（调用与出站都读内存表）；但 `/v1/models` 的**列表增删**
  要等宿主重建模型注册表——见下面的「模型变更的生效」。

### 模型变更的生效

**为什么不是即时**：禁用的「拦截」与别名的「出站翻译」都在插件内存里，改完立刻
生效（被禁用的模型当场调用会被拒）。但宿主只在 `plugin.register` / **配置重载**
时构建**模型注册表**，`/v1/models` 的增删因此要等宿主重启一次。

**控制台页的处理**：模型区块上方会常驻一条黄色提示条「模型变更待生效，需重启 CPA 生效」。
手动重启后宿主重建注册表，提示自动消失（插件在 `plugin.register` 时清除该标记）。

> 页面无法重启宿主进程——控制台页是浏览器里的静态页面，且宿主 ABI 没有让插件
> 终止宿主的接口，所以这里只作提示，不提供按钮。

> Qoder 的模型 ID 用**上游 key**（`gmodel` / `qmodel_38max` / `kmodel_latest` …），
> 名称列显示 `display_name`（`GLM-5.3` / `Qwen3.8-Max` …）。觉得 key 不好记，用别名换一个即可。

## 配置项

宿主 `plugins.configs.freetier2api` 下：

| 键 | 类型 | 默认 | 说明 |
| --- | --- | --- | --- |
| `enabled_realms` | string | `cn,global` | 启用的区域；只留 `cn` 时国际版账号不会被路由到 |
| `extra_models` | string | 空 | 额外注册的模型名，逗号或空格分隔；可带 `vendor:` 前缀限定 |
| `disabled_models` | string / list | 空 | 禁用的模型清单，逗号分隔或 YAML 列表；支持厂商限定（`workbuddycn:glm-5.2`）、区域别名（`cn:glm-5.2`）或裸名（`qwen-3.5-plus`）。与控制台页面按钮取并集 |
| `state_dir` | string | `~/.freetier2api-plugin` | 状态目录（机器盐、模型缓存、任务记录、日志）；若检测到 `~/.cli-proxy-api` 则默认用其下的 `freetier2api` 子目录（适配 Docker 挂载卷） |
| `log_level` | enum | `info` | `debug` / `info` / `error` |
| `log_to_file` | bool | `false` | 是否把日志写入 `<state_dir>/logs` |
| `workbuddy_prompt_mode` | enum | `passthrough` | `passthrough` 透传 / `custom` 替换 / `append` 追加（WorkBuddy） |
| `workbuddy_prompt_file` | string | 空 | 自定义提示词文件；留空用内置（WorkBuddy） |
| `workbuddy_sanitize_fingerprints` | bool | `true` | 出站请求体指纹脱敏（WorkBuddy） |
| `workbuddy_passthrough_ip` | bool | `false` | 是否把客户端 IP 透传给上游 |
| `workbuddy_user_agent` / `workbuddy_client_version` / `workbuddy_cli_version` | string | 空 | 出站标识覆盖（WorkBuddy） |
| `workbuddy_device_token` / `workbuddy_device_token_file` | string | 空 | `X-Device-Token` 兜底（WorkBuddy） |
| `machine_salt` | string | 自动生成 | 设备指纹盐；从旧插件迁移时填原值可保持指纹不变 |
| `auto_checkin` / `auto_checkin_at` | bool / string | **`true`** / `10:00` | 每日自动任务（默认开启） |
| `auto_tasks` | bool | **`true`** | 每日自动跑任务闭环（连登兑换、抽奖、旅行、夜猫子等） |
| `opencodezen_base_url` | string | 空 | 覆盖 OpenCode ZEN 上游基地址（默认 `https://opencode.ai/zen`） |
| `cline_base_url` | string | 空 | 覆盖 Cline 上游基地址（默认 `https://api.cline.bot/api/v1`） |
| `zcode_base_url` | string | 空 | 覆盖 ZCode **对话**上游基地址（默认 `https://api.z.ai`） |
| `zcode_claim_min_interval` | string | `15s` | 两次套餐领取的最小间隔，`0` 表示不节流 |

## 从旧插件迁移

旧插件（`workbuddy2api` / `qoder2api`）的凭证文件名是 `workbuddy-<uid>.json`、
`qoder-login-<id>.json`，`type` 字段分别是 `workbuddy` / `qoder`，本插件认不出来。

用仓库里的迁移脚本转换（**只新增文件，绝不改动原文件**——旧插件还在读它们）：

```bash
python3 scripts/migrate_auths.py /path/to/cpa/auths          # 预演
python3 scripts/migrate_auths.py /path/to/cpa/auths --apply  # 执行
```

脚本会为每个旧凭证生成一份 `<vendor>-<uid>.json`（`type` 改成 `freetier`、
补上 `vendor` 字段），原文件原样保留。两套文件并存时，宿主按 `type` 字段
路由到不同插件，互不干扰。

## 管理接口

需要 CPA 的管理密钥（`remote-management.secret-key`）。

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| GET | `/v0/management/plugins/freetier2api/status` | 账号、额度与任务状态概览（**不含 token**） |
| GET | `/vendors` | 已启用的供应商清单（供页面下拉与分组） |
| POST | `/accounts/delete` | 删除指定账号凭证文件（宿主监听 auth 目录自动注销） |
| POST | `/checkin` | 签到（body `{"account_ids":[...]}`，省略即全部） |
| POST | `/quotas` | 批量查额度 |
| GET | `/models` | 读取当前注册的模型清单（读缓存，不打上游） |
| POST | `/models/toggle` | 批量禁用/启用模型（body `{"models":[...],"disabled":true}`） |
| POST | `/models/alias` | 设置/清除某个模型的别名（body `{"model":"<vendor>:<官方ID>","alias":"对外ID"}`，`alias` 留空清除） |
| POST | `/models/refresh` | 从上游拉取模型清单并缓存 |
| GET | `/logs?since=&limit=` | 读插件日志（环形缓冲，增量拉取） |
| POST | `/settings` | 改插件运行期设置 |
| POST | `/tasks/scan` | 全账号任务扫描 |
| POST | `/tasks/run` | 执行待办队列（账号内串行、账号间并发 1-4） |
| GET | `/tasks/queue` | 队列进度 |
| POST | `/tasks/auto` | 单账号：单任务一键完成 / 一键完成全部 |
| POST | `/tasks/auto_all` | 全部账号依次一键完成（控制台页的批量按钮用它） |
| GET | `/school/status` | 开学季活动状态 |
| POST | `/travel` `/activity` `/keepalive` `/blackcat` `/school/run` | 手动触发定时任务 |
| POST | `/login/callback` | 提交用户粘贴的 localhost 授权回调链接（body 含 state 与 url 两个字段，详见 core.CallbackSubmitter） |
| GET | `/v0/resource/plugins/freetier2api/console` | 内嵌控制台页 |

控制台页是纯静态 HTML，**不含任何账号或凭证数据**；所有数据都通过上面的管理接口按需拉取。

### 密钥从哪来（为什么通常不需要再输一次）

控制台页嵌在 CPA 管理面板（CPAMC）里，与面板同源，因此直接复用面板已保存的密钥：

- 面板把整份认证状态写在 `localStorage["cli-proxy-auth"]`（`enc::v1::` XOR 混淆），
  密钥在 `state.managementKey`；
- **面板登录时必须勾选「记住密码」**，否则该键里不会包含 `managementKey`（面板本身的
  `partialize` 只在勾选时才写入）。未勾选时控制台页会把密钥输入框展开，让你手填一次，
  该值只存在本标签页（`sessionStorage`，关闭标签页即失效）；
- 旧版面板的 `managementKey` 键已不再读取。

### 封禁防护（重要）

CPA 按客户端 IP 统计管理鉴权失败次数，**未携带密钥的请求同样计数**，5 次即封禁 30 分钟，
且该封禁作用于整个 `/v0/management`（整个后台）。因此控制台页：

- 没有可用密钥时**一个请求都不发**，只展开手填框；
- 有密钥时先发**一个** `GET /vendors` 探针，成功后才串行加载首屏；
- 任一请求返回 401/403 立即停止全部自动刷新与轮询（日志 5s、队列 3s、登录轮询），
  一次打开最多只产生 1 次失败；
- 手输的密钥同样先探针验证，验证失败后 10 秒内不允许连续重试。

## 行为变更（分层重构）

- **CodeBuddy / WorkBuddy 历史账号防 Cline 误吞与兼容增强（v0.1.5）**：收紧 Cline
  凭证判定，排除别家特征（`workbuddy` 等文件名前缀、域名与特征字段）；WorkBuddy
  增强对历史未分区 `workbuddy-*.json` 凭证与域名的兼容识别，确保升级插件前添加的
  国内版与国际版账号精准归属。
- **MiniMax Code 补上上游错误分类（v0.1.4）**：此前 401/403/402/429 全部退化成 502
  可重试，坏凭证不会被停用、余额耗尽不会长冷却。现在 HTTP 状态码与流内业务码
  （如 `50110:insufficient balance`）统一走 `Classify`，凭证失效 → 401 停用、余额耗尽
  → 402、限流 → 429、5xx → 502 换号重试。
- **实例隔离补全（v0.1.4）**：Trae 的回调监听与会话归属、ZCode 的领取节流窗口
  从包级变量改为实例状态。此前同进程多个 App 会互相阻塞领取、互相认领登录回调。
- **供应商包结构统一（v0.1.3）**：文件名收敛为封闭白名单（见「架构：按供应商插桩」），
  qoder 的 `bridge`/`cosy`/`qoderapi` 三个子包扁平化进顶层包，并删除了零引用的
  `OwnsLoginSession`（6 家 × 2 处）。属纯重构：13 个实例的 ID/名称/区域、配置键、
  模型清单、凭证判定与错误映射全部保持原样，由 `tests/integration/vendor_compat_test.go`
  与各包回归锁定。
- **上游错误按供应商分类映射为宿主状态码**：401/403（凭证失效）、402（额度耗尽）、
  429（限流）与 5xx（上游抖动）会如实上报，宿主据此禁用/冷却账号。
  重构前除 WorkBuddy 外的供应商错误统一退化为 502 可重试，坏凭证不会被标记失效。
- **登录会话 id 不再用于猜供应商**：会话归属由持有会话表的供应商自证，
  删除了按 `trae_solo_` / `workbuddy_` 前缀猜测的兜底路径。
- 供应商专属配置键（`*_base_url`、WorkBuddy 的 UA/设备字段等）改由各供应商包解析，
  **键名、默认值与语义保持不变**。

## 排障与已知上游偏差

### realm 隔离

realm 隔离是本插件最容易失效的机制，已逐环节验证（宿主源码）：

| 环节 | 证据 |
| --- | --- |
| 插件按凭证注册模型 | `sdk/cliproxy/service_executors.go:454` `RegisterClient(a.ID, providerKey, models)` |
| 选凭证时按模型过滤 | `sdk/cliproxy/auth/conductor_selection.go:1202/1294/1756/2090/2314` → `:993` `ClientSupportsModel(auth.ID, routeKey)` |
| 入站模型 → provider | `internal/registry/model_registry.go:1585`（map 精确匹配） |
| `cn:` 冒号安全性 | `internal/thinking/suffix.go:25` 只解析尾部括号 |
| `model.for_auth` 生效前提 | `internal/pluginhost/adapters.go:317` 必须声明 `executor_model_scope` 为 `oauth`/`both` |
| 插件错误 → 宿主处置 | `internal/pluginhost/rpc_client.go:368` 只保留 `code`/`http_status`，只有 `http_status` 影响冷却 |
| (凭证, 模型) 级冷却 | `sdk/cliproxy/auth/conductor_cooldown.go:872`（429 天然是 per-(auth, model)） |

`tests/integration/plugin_models_test.go` 的 `TestModelsForAuthIsolatesRealms`
与 `tests/integration/registry_test.go` 的 `TestCapabilitiesMatchImplementedHandlers`
把这些契约锁进了回归。

### sanitize 正则的上游偏差

WorkBuddy 的 `sanitize.go` 移植自上游，但上游三个正则的实际内容与注释不符：

| 名字 | 实际行为 |
| --- | --- |
| `sanitizeHdrRe` | 只匹配行尾片段，并不含键名，删不掉整段键值 |
| `sanitizeKvRe` | 只删 `cc_` 前缀键值，不是通用 billing header |
| `sanitizeBareHdrRe` | 只匹配 `x-anthropic-billing-hdr` 这一个键名 |

本插件按注释所载**意图**实现（`sanitize_rules.json` 中按「键名 + 后续键值段整段删除」表达），
其余两条沿用原样。若上游修正正则，需要同步 `internal/vendors/workbuddy/assets/sanitize_rules.json`。
该 JSON 是程序化提取生成的，以保证字节精确；修改后用
`internal/vendors/workbuddy/payload_test.go` 的用例回归。

### 多插件共存与 priority

宿主解析凭证时按插件遍历，**一旦某个插件报错或认领就中止**，且文件没有 `type` 时会把
当前询问插件的 identifier 兜底填进 `provider`。因此「provider 等于自己就认领」的插件
会吞掉所有文件。遍历顺序是 `priority` 降序、同优先级按 `id` 升序；本插件需要更高
`priority` 才能优先被询问：

```yaml
plugins:
  configs:
    freetier2api:
      priority: 100
```

### glibc / musl

本插件是 cgo 构建的 c-shared 动态库，libc 必须与宿主一致。alpine（musl）产物在
Debian（glibc）宿主上会报 `libc.musl-*.so.1: cannot open shared object file`。
请用 `golang:1.24`（Debian）或与宿主同族的镜像构建；`scripts/godocker.sh` 已固定。

### 控制台页与封禁

CPA 对 `/v0/management` 的鉴权失败计数包含**未带密钥**的请求，5 次封禁 30 分钟。
控制台页因此只在拿到有效密钥后发**一个** `GET /vendors` 探针，成功后才串行加载；
无密钥时一个请求都不发；401/403 后停止全部自动刷新。

### 需要真实环境验证的部分

无法在无 CPA 的环境下验证以下内容：`plugin.register` 实际往返、真实上游模型清单、
真实对话出字、签到/余额/任务上报的真实返回、双域端到端隔离。按上一节「测试与验证」
的 curl 顺序逐项确认。

## 已知限制

- **模型清单变更后需要一次宿主重载**：宿主的模型注册表只在插件加载/配置重载时读取。
  禁用/启用的**调用拦截**与别名的**出站翻译**是即时的，只有 `/v1/models` 列表要等重载；
  控制台页的「应用变更」可触发软重载（见「模型变更的生效」）。
- **同名模型跨供应商合并**：宿主按 `EqualFold` 合并，选号可能落到另一家。
  用 `vendor:model` 精确指定。
- **WorkBuddy 国际版签到 = 每日活跃奖励**：国际版没有国内版的 daily-checkin
  活动，每日收入路径是活跃信标——向计费域 `POST /v2/report` 发一条
  `chat_request_send` 事件，把当天成长热力图点亮；次日凌晨自动发放 30 积分
  Bonus Pack，连续点亮解锁连登档位。实现见 `internal/vendors/workbuddy/tasks.go`：
  以热力图最后一格判定「今天」，点亮即幂等跳过；信标不重试（事件按天幂等，
  盲发会把计数翻倍），单次失败由 8/12/16/20 点的排程兜底，也可经管理端
  `POST /plugins/freetier2api/dailybonus` 手动触发。**成长任务仍只在国内版提供**，
  国际版入口会被跳过（不报错、不发请求）。**Qoder 国际版没有签到计划**：
  这些活动只在国内版提供。
- **ZCode 领取依赖浏览器人机验证**：验证码在控制台页面内完成（不内置免浏览器求解器）；
  跨出口部署时可能因阿里云 IP 校验失败，需在同网络下提交。
- **ZCode 没有签到**：其「签到」实质是领取活动套餐，控制台提供手动领取入口
  无痕验证码（无图片可作答），本插件不内置浏览器求解器。额度查询不受影响。
- **Qoder 上游没有配额重置接口**：额度按计费周期自动恢复。
- **部分 WorkBuddy 任务不可自动化**：`Expert_Philanthropy` 需要真实捐款
  （服务端校验捐赠回执）。
- **宿主「OAuth 模型禁用」弹窗列不出本插件的模型**：该弹窗读的是宿主内置的
  `/model-definitions/<channel>`，其 channel 是**硬编码的原生 provider 白名单**，
  插件的 provider 不在其中，因此会显示「无法获取模型列表」。这是宿主限制，
  与插件无关。**请用本插件控制台页的「模型」区块查看模型清单。**
- **上游风控依赖设备指纹**：换机器或换状态目录会被视为新设备。

## 许可

GPL-3.0（并入的 Qoder 部分源自 GPL-3.0 的 qoder2api / QCCG）。
第三方来源与署名见 [`THIRD_PARTY_NOTICES.md`](THIRD_PARTY_NOTICES.md)。
