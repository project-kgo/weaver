# Weaver 项目协作指南

## 沟通与代码风格

- 统一使用中文回复。
- Go 代码遵循标准习惯，提交前执行 `gofmt`。
- 注释使用中文，只解释设计意图、约束或不直观的行为，不给显而易见的代码逐行配注释。
- 优先使用标准库和已有依赖；没有当前需求时，不引入新的库、抽象层或兼容层。

## 项目介绍

Weaver 是一个嵌入业务进程的轻量 ConnectRPC 部署感知运行时。它允许同一份业务代码和同一个二进制，仅通过启动配置选择单体部署或多 unit 部署。

项目中的两个基本边界是：

- protobuf service 是逻辑组件边界。
- unit 是部署、进程和故障边界。

组件之间通过 `Ref[T]` 依赖：组件和调用方在同一 unit 时直接调用 Go 实现；位于不同 unit 时，由 Runtime 自动创建 ConnectRPC Client。业务代码不应出现 local/remote 判断。

组件 YAML 配置通过匿名嵌入 `WithConfig[T]` 注入，并按 protobuf service 全名匹配配置段。普通进程内依赖通过 `Resource[T]` 注入，例如数据库连接和第三方 Client。资源按精确 Go 类型匹配，其创建和关闭由业务启动代码负责，Weaver 不接管资源生命周期。

主要入口：

- `component.go`：`Implements[T]`、`Ref[T]`、`WithConfig[T]`、`Resource[T]` 和生命周期接口。
- `runtime.go`：组件创建、注入、拓扑初始化、Handler 聚合和关闭。
- `resolver.go`：unit target 解析协议。
- `registry.go`：生成代码使用的组件注册表。
- `internal/protocgen`：`protoc-gen-weaver-go` 实现。
- `internal/generate`：`weaver generate` Go 源码扫描器。
- `examples/echo`：同一二进制的单体和双 unit 示例。

基线为 Go 1.26.2、Connect Go 和标准 `net/http`。

## 核心设计思想

### 部署不侵入业务

业务只负责：

1. 定义 unary protobuf service。
2. 编写 Service 的普通 Go 实现。
3. 使用 `Implements[T]`、`Ref[T]`、`WithConfig[T]` 和 `Resource[T]` 声明关系。

Service 注册、字段赋值、Handler 挂载、远程 Client 创建和本地代理均由生成代码与 Runtime 完成。不要把这些基础设施逻辑重新放回业务代码。

### 生成优于运行时猜测

能够在构建期确定的关系应由生成器确定：

- `protoc-gen-weaver-go` 生成 Service 描述、本地代理、远程 Client 工厂和 Handler 工厂。
- `weaver generate` 扫描组件声明，生成工厂、注入逻辑、依赖元数据和编译期接口检查。
- 生成结果必须稳定排序，连续生成不得产生差异。
- 生成代码包含 `CodegenVersion` 断言，旧生成代码不能静默配合新 Runtime 运行。

不要手工修改 `*.weaver.go`、`*.connect.go`、`*.pb.go` 或 `zz_weaver_gen.go`。需要变化时修改源 proto、业务声明或生成器，然后重新生成。

### 启动时确定，运行时保持简单

- 配置描述 `units`、`placements` 和按 protobuf service 全名索引的组件配置段，启动后不可变。
- `currentUnit` 必须由启动参数或 `APP_UNIT` 明确提供，不做自动推断。
- Runtime 先严格校验全部组件配置，再创建当前 unit 的全部组件，注入 `WithConfig`、`Resource` 和 `Ref`，随后按依赖拓扑执行 `Init`。
- 循环依赖、未知组件、缺失 placement、未知配置段或字段、配置类型错误、缺失或重复资源必须在启动阶段失败。
- 初始化失败时逆序清理已经成功初始化的组件；正常退出时逆序执行 `Shutdown`。
- Resolver 每个远程 unit 在启动期间只解析一次并缓存结果。动态实例发现和负载均衡属于 Resolver 返回的 `HTTPClient` 或 `RoundTripper`。

### 本地与远程语义一致

- 本地代理保留已有的 `*connect.Error`。
- 普通错误转换为 Connect `CodeUnknown`。
- `context.Canceled` 和 `context.DeadlineExceeded` 分别映射为对应 Connect 错误码。
- Service 实现和 Handler 都不得修改调用方传入的 protobuf request。
- 鉴权规则、参数校验和领域错误等本地与远程必须一致的逻辑应放在 Service 实现中。Connect Client/Handler option 只处理传输层能力。

## 简洁设计与不过度设计的边界

实现新需求时，先寻找满足当前验收条件的最小闭环。不要因为“以后可能需要”提前建设通用平台。

v0.1 明确不做：

- Streaming RPC 的透明本地/远程桥接；生成器遇到 Streaming 必须明确报错。
- 独立控制面、调度器、自动扩缩容、动态迁移或配置热更新。
- Runtime 内建服务注册、动态负载均衡、重试、熔断、限流或完整可观测性平台。
- 在核心包内直接实现 Consul、Kubernetes 或 etcd；这些能力应作为独立 Resolver 适配包存在。
- 命名资源、多实现选择、Provider 依赖图或通用依赖注入容器。
- 重新实现 ConnectRPC 协议、编码、Client 或 Handler；始终复用 Connect 生成结果。
- 为了兼容尚不存在的使用方保留重复 API、别名层或配置格式。

判断是否过度设计时采用以下标准：

1. 当前需求或测试是否真实需要它？如果不是，不实现。
2. 标准库、Connect 或现有的小接口是否已经解决问题？如果是，直接复用。
3. 是否能通过一个明确的数据结构或函数完成？如果可以，不引入框架、插件系统或多层接口。
4. 新抽象是否至少消除了当前存在的重复或耦合？仅为潜在未来预留的抽象不接受。
5. 能在适配包完成的能力，不下沉到核心 Runtime。

保持接口小而具体。仅在出现真实的第二种实现或稳定变化轴后再抽象，不提前设计扩展点。

## 代码生成约定

标准开发流程：

```text
buf generate
weaver generate ./...
go build ./...
```

- `protoc-gen-connect-go` 必须使用 `simple` 模式。
- protobuf 插件只支持 unary 方法，并直接复用 Connect 生成的接口与构造函数。
- Go 扫描器使用 `go/packages` 识别匿名嵌入的 `Implements[T]`、`WithConfig[T]` 以及 `Ref[T]`、`Resource[T]` 字段。
- 一个 protobuf Service 只能有一个组件实现。
- 生成文件必须提交仓库。CI 会重新生成并执行 `git diff --exit-code`。
- 修改生成器时必须更新对应 golden test，并验证重复生成无差异、生成代码可编译。

## 修改与验证要求

修改前先确认变化属于 Runtime、protobuf 生成器、Go 扫描器还是独立适配层，避免跨层堆叠职责。

完成代码修改后至少执行：

```bash
gofmt -w <修改的 Go 文件>
go test ./...
go build ./...
```

涉及 Runtime 并发、HTTP 或生命周期时执行：

```bash
go test -race ./...
```

涉及生成器时还应：

- 运行相关 golden test。
- 对相同输入连续生成两次并确认无差异。
- 重新生成示例并执行 `git diff --exit-code`。
- 验证非法声明、过期版本和 Streaming 拒绝行为。

新增 Runtime 行为应优先覆盖失败路径和语义一致性，包括配置错误、依赖循环、资源错误、初始化回滚、逆序关闭、Resolver 缓存、错误码、取消、超时和 request 不可变性。

不要为了让测试通过而削弱启动校验，也不要在业务示例中手工完成本应由生成代码负责的注册、挂载或注入。
