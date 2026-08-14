# Weaver

Weaver 是一个很薄的 ConnectRPC 部署感知运行时。它让同一份 Go 代码、同一个二进制可以按配置运行成单体或多个部署单元，同时让业务代码保持不变。

核心原则只有三条：

1. protobuf service 是组件边界，unit 是部署与故障边界。
2. 同 unit 直接调用 Go 实现，跨 unit 使用基于 HTTP/2 的 ConnectRPC。
3. 注册、Handler 挂载、Client 创建和依赖注入由代码生成完成。

它不是 Service Weaver 的重写，也不负责调度、扩缩容、动态迁移或通用服务注册。内置 Kubernetes Resolver 只负责根据 EndpointSlice 发现 Pod，并在客户端执行轻量负载均衡。

## 开发体验

定义 unary protobuf service，并让 Connect 使用 `simple` 模式生成代码：

```yaml
plugins:
  - local: protoc-gen-go
    out: gen
    opt: [paths=source_relative]
  - local: protoc-gen-connect-go
    out: gen
    opt: [paths=source_relative, simple]
  - local: protoc-gen-weaver-go
    out: gen
    opt: [paths=source_relative]
```

业务实现只声明组件、依赖、组件配置和普通资源：

```go
type Settings struct {
    Prefix string `yaml:"prefix"`
}

type echoService struct {
    weaver.Implements[examplev1weaver.EchoServiceComponent]
    weaver.WithConfig[Settings]
    Upper    weaver.Ref[examplev1weaver.UpperServiceComponent]
    Database weaver.Resource[*sql.DB]
}
```

`WithConfig[T]` 必须匿名嵌入，`T` 必须是结构体。Runtime 会在 `Init` 前注入配置，通过 `Config() *T` 访问；缺少配置段时注入零值。

生成并构建：

```bash
buf generate
weaver generate ./...
go build ./...
```

生成的 `*.weaver.go` 和 `zz_weaver_gen.go` 应提交仓库。普通 `go build` 不依赖生成工具，CI 通过重新生成和 `git diff --exit-code` 检查产物是否过期。

## Runtime

```go
config, err := weaver.ParseConfig(data)
if err != nil {
    return err
}

runtime, err := weaver.New(
    ctx,
    os.Getenv("APP_UNIT"),
    config,
    weaver.WithResource(database),
    weaver.WithResolver("consul", consulResolver),
)
```

配置使用 protobuf service 全名：

```yaml
units:
  core: consul://core
  game: http://game.internal:8080

placements:
  game.wallet.v1.WalletService: core
  game.table.v1.TableService: game

game.wallet.v1.WalletService:
  currency: CNY
```

组件配置段必须使用 `placements` 中的 protobuf service 全名。配置字段支持 `yaml` 标签；未知配置段、未知字段和类型错误都会导致启动失败。

组件配置的字符串值支持使用 `${NAME}` 引用环境变量，也可以嵌入到其他文字中，例如 `dsn: 'postgres://${DB_USER}:${DB_PASSWORD}@db/app'`。未设置的环境变量或不完整的引用会使 `ParseConfig` 失败；环境变量值始终作为字符串处理，不会被重新解析成 YAML 结构。

`http` 和 `https` 使用内置静态 Resolver。`kube` 使用内置 Kubernetes Resolver，无需调用 `WithResolver`；其他 scheme 仍通过 `WithResolver` 注册。Resolver 返回的 `HTTPClient` 自行负责实例变化、连接池和负载均衡，Weaver 只在启动阶段解析并缓存目标。

自定义 Resolver 可以复用公共 `balancer` 包管理动态 endpoint。发现层只需在地址变化时调用 `Update`，并在关闭时调用 `Close`：

```go
pool, err := balancer.New(
    newEndpointClient, // 根据地址创建独立的 *http.Client
    balancer.WithRetryableError(isDialError),
)
if err != nil {
    return err
}
if err := pool.Update(addresses); err != nil {
    return err
}
resolved := weaver.ResolvedTarget{
    BaseURL:    "http://game.service",
    HTTPClient: pool,
}
```

`balancer` 使用 P2C 从两个随机候选中选择得分更低的 endpoint。得分同时考虑延迟 EWMA、基础设施错误 EWMA 和在途请求数；默认 EWMA 半衰期为 10 秒、错误惩罚为 1 秒、最多尝试 2 个不同 endpoint。只有调用方明确标记的安全错误才会切换 endpoint 重试；除 `502`/`503`/`504` 外的 HTTP 或业务响应不会降低节点权重。

内置静态 Resolver 的默认 Client 强制使用 HTTP/2：`http://` 目标使用明文 h2c prior knowledge，`https://` 目标使用 TLS HTTP/2，不会在连接失败后回退到 HTTP/1.1。远程 unit 因此必须启用对应的 HTTP/2 支持。通过 `WithHTTPClient` 或自定义 Resolver 提供 Client 时，调用方负责保证 Client 支持目标所需的 HTTP/2 传输。

Kubernetes target 使用 `kube://<namespace>/<service>:<port>`：

```yaml
units:
  core: kube://production/weaver-core:connect
  game: kube://production/weaver-game:8080
```

端口名从 EndpointSlice 的命名端口解析，数字端口表示 Pod 实际监听端口。默认使用 h2c；通过 `?transport=https` 使用 TLS，并以 `<service>.<namespace>.svc` 作为 ServerName。Runtime 会在启动时收集当前 unit 实际依赖的全部 `kube` target；同一 namespace 的 Service 合并为一套带 `labelSelector` 的分页 LIST/WATCH，不读取无关 Service。不同 namespace 的 WATCH 共用一个优先使用 HTTP/2 的 Kubernetes Client，HTTP/2 可用时会复用底层连接。

业务请求按 Pod 复用独立 HTTP/2 连接，并通过 P2C 与延迟/错误 EWMA 动态选择 endpoint。控制面暂时断开时保留最后一次有效结果；LIST 和 WATCH 都有客户端超时保护，WATCH 到期或半断开后自动重连，`resourceVersion` 过期时自动重新 LIST。

Pod 的 ServiceAccount 只需要在涉及的 namespace 中读取 EndpointSlice。以下 `Role` 和对应的 `RoleBinding` 需要在每个目标 namespace 部署一次：

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: weaver-endpointslice-reader
  namespace: production
rules:
  - apiGroups: ["discovery.k8s.io"]
    resources: ["endpointslices"]
    verbs: ["list", "watch"]
```

内置 Resolver 从标准 ServiceAccount token 和 CA 文件读取凭证，只在首次使用 `kube` target 时初始化，并由 Runtime 在正常退出或启动失败时自动关闭。EndpointSlice 暂时没有 ready endpoint 不阻止 Runtime 启动；调用会返回无可用 endpoint，发现结果更新后自动恢复。

组件创建顺序为：严格校验全部组件配置、创建全部本地实例、注入 `WithConfig`/`Resource`/`Ref`、按依赖顺序执行 `Init`、挂载当前 unit 的 Handler。关闭时按相反顺序执行 `Shutdown`。普通资源由调用方管理生命周期。

## Recovery 与 OpenTelemetry

Runtime 默认启用 recovery、OpenTelemetry trace 和 metric。Weaver 使用 OpenTelemetry 的全局 `TracerProvider`、`MeterProvider` 和 `TextMapPropagator`，应用应在调用 `weaver.New` 前完成配置，并自行关闭 Provider 和 Exporter；未配置时 OpenTelemetry API 保持 no-op，Weaver 不会启动独立采集或导出进程。

可以使用 `NewTracerProvider` 创建常规 OpenTelemetry SDK Provider，并通过原生 SDK 选项配置 exporter、Resource 或覆盖采样器。默认采样器平均每秒采样一条由当前进程发起的根 trace，子 span 跟随父 span 的采样决定：

```go
tracerProvider := weaver.NewTracerProvider(
    sdktrace.WithBatcher(exporter),
    sdktrace.WithResource(serviceResource),
)
otel.SetTracerProvider(tracerProvider)
otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
    propagation.TraceContext{},
    propagation.Baggage{},
))
defer tracerProvider.Shutdown(context.Background())
```

业务可以传入 `sdktrace.WithSampler` 覆盖默认采样策略。Provider 只负责 trace，MeterProvider、Propagator、Exporter 的创建和关闭仍由应用负责。

跨 unit Connect 调用同时生成 client/server span 和标准 RPC 指标。unit 之间按内部服务处理并信任传播的 trace context，使 server span 成为 client span 的子节点；因此对外暴露 `Runtime.Handler()` 时，应用必须在外围完成可信边界、鉴权和流量隔离。服务端 peer 地址不会写入埋点，避免临时端口形成高基数。

同 unit 调用由生成的本地代理直接完成 recovery 和埋点，不经过 Connect interceptor。每次调用生成一个 INTERNAL span，以及名为 `weaver.local.call.duration`、单位为秒的耗时直方图。span 和指标只使用 protobuf service、method 与有限的 Connect 结果码作为维度。

组件实现或 Handler interceptor 发生 panic 时，调用方只会收到 `connect.CodeInternal`，不会看到 panic 内容。Weaver 会通过 `slog.Default()` 记录 procedure、panic 原因和堆栈，并向当前 span 写入 exception 事件；日志输出与生命周期仍由应用配置。

可以分别为跨 unit Client 和当前 unit Handler 注入 Connect interceptor：

```go
runtime, err := weaver.New(
    ctx,
    unit,
    config,
    weaver.WithClientInterceptors(clientInterceptor),
    weaver.WithHandlerInterceptors(handlerInterceptor),
)
```

Client 调用顺序为“内置 OTel → 用户 Client interceptor → transport”，Handler 调用顺序为“内置 OTel → recovery → 用户 Handler interceptor → Service”。多次配置按传入顺序追加；用户 interceptor 不会在同 unit 本地调用中执行。需要本地与远程保持一致的鉴权、校验和领域逻辑仍应放在 Service 实现中。

Handler interceptor 需要调用组件时，可以按组件接口类型延迟创建。工厂收到的组件与 `Ref[T]` 一样：同 unit 时是本地代理，跨 unit 时是远程 Client，并复用 Runtime 中的同一个代理对象：

```go
weaver.WithHandlerInterceptor[examplev1weaver.LoginServiceComponent](
    func(login examplev1weaver.LoginServiceComponent) connect.Interceptor {
        return newAuthInterceptor(login)
    },
)
```

Connect 生成的 Handler 会在同一路径上自动接受 Connect、gRPC 和 gRPC-Web。默认可通过 `Serve` 启动同时支持 HTTP/1、HTTP/2 和明文 h2c 的服务。在 Unix 系统上，`Serve` 会监听 `SIGINT`、`SIGTERM`、`SIGHUP` 和 `SIGQUIT`；收到退出信号或 `ctx` 结束后，依次优雅关闭 HTTP 服务和 Runtime：

```go
err := weaver.Serve(ctx, unit, config, ":8080", options...)
```

通过 `WithShutdownHook` 可以注册数据库等外部资源的清理逻辑。组件会先按依赖逆序关闭，随后多个 hook 按注册顺序的逆序执行；所有关闭错误都会合并返回：

```go
err := weaver.Serve(
    ctx,
    unit,
    config,
    ":8080",
    weaver.WithResource(database),
    weaver.WithShutdownHook(func(context.Context) error {
        return database.Close()
    }),
)
```

需要自定义 TLS、超时或外围 Handler 时，仍可使用 `weaver.New` 获取 `Runtime.Handler()` 并自行管理 `http.Server`。

## 示例

先安装 Buf 和三个生成器：

```bash
brew install bufbuild/buf/buf
go install google.golang.org/protobuf/cmd/protoc-gen-go@v1.36.11
go install connectrpc.com/connect/cmd/protoc-gen-connect-go@v1.20.0
go install ./cmd/protoc-gen-weaver-go
go install ./cmd/weaver
```

重新生成示例：

```bash
cd examples/echo
buf generate
cd ../..
weaver generate ./examples/echo/internal/app
```

单体运行：

```bash
go run ./examples/echo/cmd/server \
  -unit app \
  -config examples/echo/config/monolith.yaml \
  -listen :8080
```

拆成两个 unit：

```bash
go run ./examples/echo/cmd/server -unit core -config examples/echo/config/microservices.yaml -listen :8081
go run ./examples/echo/cmd/server -unit game -config examples/echo/config/microservices.yaml -listen :8082
```

调用 Echo：

```bash
curl -H 'Content-Type: application/json' \
  -d '{"value":"hello"}' \
  http://127.0.0.1:8082/weaver.example.v1.EchoService/Echo
```

也可以从仓库根目录通过明文 HTTP/2 使用 gRPC 协议调用同一个 Handler：

```bash
buf curl \
  --schema examples/echo \
  --protocol grpc \
  --http2-prior-knowledge \
  --data '{"value":"hello"}' \
  http://127.0.0.1:8082/weaver.example.v1.EchoService/Echo
```

HTTPS 地址不需要 `--http2-prior-knowledge`，Client 会通过 TLS ALPN 协商 HTTP/2。两种调用都会返回 `{"value":"echo:HELLO"}`。

## v0.1 边界

- 仅支持 unary RPC；生成器遇到 Streaming 会失败。
- placement 在启动后不可变，调整部署需要滚动重启。
- `Resource[T]` 按精确 Go 类型匹配，不支持命名资源和 Provider 图。
- 组件依赖必须是无环图。
- Connect interceptor 只处理传输层。业务校验、鉴权规则和领域错误不能只放在远程 Handler interceptor 中。
- 自定义 Connect interceptor 只作用于跨 unit Client 或入站 Handler，不作用于同 unit 本地调用。
- Handler 不得修改 request；组件边界始终按“可能经过网络”设计。
- 默认跨 unit Client 只使用 HTTP/2，不自动探测或回退到 HTTP/1.1。
