# 架构

依赖方向：`main/cgo → internal/app → internal/{admin,tasks,accounts} → internal/{vendors,modelconfig,...} → internal/core`。

- `internal/app.App` 持有配置、状态、模型、供应商注册表、登录会话与后台任务；`main.go` 只创建 C ABI 需要的默认实例。
- `core.Registry` 显式注册供应商；`core.Env{Host, Registry, Logins}` 注入给供应商工厂。
- 只有 `internal/app/vendors.go` 可以 import 具体供应商。新增供应商：新建目录、实现 `Instances(env)`、在 `vendors.go` 加一行、README 补一行。
- 供应商通过可选接口声明能力（登录/额度/签到/任务/配置字段）；不支持的供应商不实现、不建空方法。
- `tests/` 是跨包集成测试，`tests/e2e/` 是完整 ABI 端到端，`tests/live/` 是实况探针。
