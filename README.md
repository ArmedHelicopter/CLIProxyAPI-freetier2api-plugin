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

## 安装

### 通过 CPA 插件商店（推荐）

本插件已收录进 CPA 官方插件源，**无需额外配置三方源**。
在管理面板 → 插件商店 → 搜 **FreeTier 2API** → 一键安装即可。

### 手动安装

从 [Releases](https://github.com/jiandanc/CLIProxyAPI-freetier2api-plugin/releases)
下载对应平台的 `freetier2api_<版本>_<goos>_<goarch>.zip`，解压后把其中的
`freetier2api` 动态库放进 CPA 的插件目录：

| 平台 | 下载归档 | 解压出的文件 |
| --- | --- | --- |
| Linux x86_64 | `freetier2api_<版本>_linux_amd64.zip` | `freetier2api.so` |
| Linux arm64 | `freetier2api_<版本>_linux_arm64.zip` | `freetier2api.so` |
| macOS Apple Silicon | `freetier2api_<版本>_darwin_arm64.zip` | `freetier2api.dylib` |
| macOS Intel | `freetier2api_<版本>_darwin_amd64.zip` | `freetier2api.dylib` |
| Windows x64 | `freetier2api_<版本>_windows_amd64.zip` | `freetier2api.dll` |

归档内的文件名已经是宿主要求的 `freetier2api{ext}`（宿主按 `{pluginID}{ext}`
找动态库），**解压后无需改名**，直接放进插件目录即可：

```yaml
plugins:
  enabled: true
  dir: "/path/to/cpa/plugins"
  configs:
    freetier2api:
      enabled: true
      enabled_realms: "cn,global"
```

归档完整性可用 Release 里的 `checksums.txt` 核对（`<sha256>  <文件名>`）。

## 添加账号

两种方式，最终都在 CPA 的 auth 目录里产出一个 `<vendor>-<uid>.json` 凭证文件：

1. **控制台页登录**（推荐）：打开
   `http://<CPA>/v0/resource/plugins/freetier2api/console` →
   「添加账号 ▾」→ 从下拉里选供应商 → 在浏览器里完成设备授权。
2. **手动放凭证文件**：按下表填好 JSON 放进 CPA 的 auth 目录。

**文件名以供应商 ID 开头**最省事（如 `workbuddycn-myaccount.json`）；否则请在文件里
带上 `type` 与 `vendor` 两个字段，让插件能确定归属：

```json
{
  "type": "freetier",
  "vendor": "workbuddycn"
}
```

| 供应商 | 必填字段 | 建议 / 可选字段 |
| --- | --- | --- |
| WorkBuddy 国内版 / 国际版 | `auth.accessToken` | `auth.refreshToken`（自动续期必需）、`auth.expiresAt`、`auth.domain`、`auth.realm`、`account.uid`（指纹种子）、`device_token`；顶层扁平写法同样支持 |
| Qoder 国内版 / 国际版 | `device_token`（`dt-` 前缀；`pt-` PAT 亦可） | `refresh_token`（`drt-` 前缀）、`region`（`cn` / `global`）、`secret`（qoder2api 导出格式） |
| OpenCode ZEN | `api_key`（`apikey` / `zen_key` 亦兼容） | `label`（缺失时自动脱敏显示） |
| Cline | `refreshToken`（WorkOS 设备授权产物） | `accessToken`、`email` |
| ZCode | 对话要 `api_key`（智谱格式 `xxx.yyy`）；查额度要 `jwt_token` | `profile`（每账号独立设备档案）、`email`、`user_id`、`label`；`auth_mode: oauth` |
| TRAE SOLO | `access_token` | `refresh_token`、`uid`（签到设备 ID 与指纹）、`nickname` |
| Trae 国内版 / 国际版 | `access_token` | `refresh_token`、`uid`、`vendor`（`traecn` / `traeglobal`）、`region`（`cn` / `global`） |
| CodeArts | `access_key_id`、`secret_access_key`、`security_token` | `refresh_token`、`dpop_private_key`（P-256 ES256 JWK，续期用） |
| MiniMax Code 国内版 / 国际版 | `access_token` | `refresh_token`、`user_id`、`email`、`label`、`region` |

> ZCode 的**活动套餐领取**需要阿里云无痕验证码，本插件不内置求解器：控制台页面会渲染
> 验证码控件，用户通过后插件只做转发。**对话一律走 `api_key` 通道**，与验证码无关。

**Trae / CodeArts 卡在「等待授权中…」**：这两家上游只认本机回调地址，CPA 与浏览器不在
同一台机器（远程主机、Docker、WSL）时回调到不了。展开登录框里的「回调没到插件？
粘贴回调链接」，把地址栏里的整条链接粘进去提交即可。

## 模型别名与跨供应商路由

模型用**裸名**（不带供应商前缀）调用；跨供应商同名模型由宿主按 `strings.EqualFold`
合并成一个条目，再从各家凭证里选号。需要精确指定某一家时用 `vendor:model`
（如 `workbuddycn:glm-5.2`）。

在控制台页「模型」区块，**点击某行的「别名」单元格**即可编辑：回车或点开别处保存，
Esc 取消；清空保存即清除别名。每个供应商的每个模型各自独立设置。别名就是该模型
注册给宿主的对外模型 ID，客户端以别名调用，出站前插件再还原成上游的官方 ID。

这带来一个关键能力：**把不同供应商的不同模型设成同一个别名**。宿主会把这个
别名当作一个模型，从这几家凭证里选号——任意一家可用即可应答，天然跨供应商
故障转移。例如给 `workbuddycn:glm-5.2` 与 `qodercn:glm-5.3` 都设别名 `glm-max`，
客户端只请求 `glm-max`，网关会在两家之间自动调度。

- 作用是**供应商内的单个模型 ID**：同名的官方 ID 出现在别的供应商时互不影响，
  各家可各自起别名。
- 别名在**同一供应商内唯一**：把某别名换绑到新模型时，该供应商下占用同名别名
  的旧模型会恢复官方 ID。
- 别名留空即清除，模型恢复用官方 ID 对外。
- 别名**即时生效**（调用与出站都读内存表）；但 `/v1/models` 的**列表增删**
  要等宿主重建模型注册表——见下面的「模型变更的生效」。

## 模型变更的生效

**为什么不是即时**：禁用的「拦截」与别名的「出站翻译」都在插件内存里，改完立刻
生效（被禁用的模型当场调用会被拒）。但宿主只在 `plugin.register` / **配置重载**
时构建**模型注册表**，`/v1/models` 的增删因此要等宿主重启一次。

**控制台页的处理**：模型区块上方会常驻一条黄色提示条「模型变更待生效，需重启 CPA 生效」。
手动重启后宿主重建注册表，提示自动消失（插件在 `plugin.register` 时清除该标记）。

> 页面无法重启宿主进程——控制台页是浏览器里的静态页面，且宿主 ABI 没有让插件
> 终止宿主的接口，所以这里只作提示，不提供按钮。

> Qoder 的模型 ID 是上游 `display_name` 的规范化形式（小写、空格转 `-`），
> 如 `GLM-5.3` → `glm-5.3`、`DeepSeek-V4-Pro` → `deepseek-v4-pro`、`Qwen3.8-Max` → `qwen3.8-max`。
> 出站时插件会按上游清单把它还原成 Qoder 网关要求的内部 key（`gmodel` / `dmodel` …），
> 客户端无需关心。仍可直接传旧 key（`gmodel`、`dmodel`），插件一并接受。

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
3. 在本文件的供应商表与「添加账号」的凭证字段表里各补一行；如有区域/provider key 差异，
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
  Qoder 国内版与国际版均支持每日签到（领取每日福利活动，见下）。
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
| `traecn_ide_version` / `traecn_ide_version_code` | string | 内置 | 出站 IDE 版本覆盖，键名按实例区分（`traecn` / `traeglobal` / `traesolo`）。上游按构建号下发不同模型目录，构建号过低会看不到新模型（Trae） |
| `workbuddy_device_token` / `workbuddy_device_token_file` | string | 空 | `X-Device-Token` 兜底（WorkBuddy） |
| `machine_salt` | string | 自动生成 | 设备指纹盐；填原值可保持指纹不变 |
| `auto_checkin` / `auto_checkin_at` | bool / string | **`true`** / `10:00` | 每日自动任务（默认开启） |
| `auto_tasks` | bool | **`true`** | 每日自动跑任务闭环（连登兑换、抽奖、旅行、夜猫子等） |
| `opencodezen_base_url` | string | 空 | 覆盖 OpenCode ZEN 上游基地址（默认 `https://opencode.ai/zen`） |
| `cline_base_url` | string | 空 | 覆盖 Cline 上游基地址（默认 `https://api.cline.bot/api/v1`） |
| `zcode_base_url` | string | 空 | 覆盖 ZCode **对话**上游基地址（默认 `https://api.z.ai`） |
| `zcode_claim_min_interval` | string | `15s` | 两次套餐领取的最小间隔，`0` 表示不节流 |

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

## 已知限制

- **模型清单变更要等宿主重载**：`/v1/models` 的增删在宿主重建注册表后才生效（别名与禁用的调用拦截是即时的）。
- **同名模型跨供应商合并**：宿主按 `EqualFold` 合并后选号，可能落到另一家；需要精确指定时用 `vendor:model`。
- **WorkBuddy 国际版签到**是每日活跃信标，次日发放积分；成长任务仍只在国内版提供。
- **Qoder 签到**是领取每日福利活动，窗口每日 10:00（UTC+8）开启、错过不补领。
- **Qoder 任务中心为空**：上游没有可自动推进的日常任务接口，签到即覆盖全部可自动化收益。
- **ZCode 套餐领取**依赖浏览器内的人机验证，跨出口部署可能因 IP 校验失败。
- **ZCode 没有签到**：其「签到」实质就是领取活动套餐。
- **Qoder 没有配额重置接口**：额度按计费周期自动恢复。
- **WorkBuddy 的 `Expert_Philanthropy` 任务不可自动化**：需要真实捐款回执。
- **宿主「OAuth 模型禁用」弹窗列不出本插件的模型**：宿主只认内置 provider 白名单，请用本插件控制台页的「模型」区块。
- **上游风控依赖设备指纹**：换机器或换状态目录会被视为新设备。

## 许可

GPL-3.0（并入的 Qoder 部分源自 GPL-3.0 的 qoder2api / QCCG）。
第三方来源与署名见 [`THIRD_PARTY_NOTICES.md`](THIRD_PARTY_NOTICES.md)。
