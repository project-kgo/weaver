# Weaver

Weaver 是一个很薄的 ConnectRPC 部署感知运行时。它让同一份 Go 代码、同一个二进制可以按配置运行成单体或多个部署单元，同时让业务代码保持不变。

核心原则只有三条：

1. protobuf service 是组件边界，unit 是部署与故障边界。
2. 同 unit 直接调用 Go 实现，跨 unit 使用基于 HTTP/2 的 ConnectRPC。
3. 注册、Handler 挂载、Client 创建和依赖注入由代码生成完成。

它不是 Service Weaver 的重写，也不负责调度、扩缩容、动态迁移、服务注册或负载均衡。

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

`http` 和 `https` 使用内置静态 Resolver。其他 scheme 通过 `WithResolver` 注册；Resolver 返回的 `HTTPClient` 自行负责实例变化、连接池和负载均衡。Weaver 只在启动阶段解析并缓存目标。

内置静态 Resolver 的默认 Client 强制使用 HTTP/2：`http://` 目标使用明文 h2c prior knowledge，`https://` 目标使用 TLS HTTP/2，不会在连接失败后回退到 HTTP/1.1。远程 unit 因此必须启用对应的 HTTP/2 支持。通过 `WithHTTPClient` 或自定义 Resolver 提供 Client 时，调用方负责保证 Client 支持目标所需的 HTTP/2 传输。

组件创建顺序为：严格校验全部组件配置、创建全部本地实例、注入 `WithConfig`/`Resource`/`Ref`、按依赖顺序执行 `Init`、挂载当前 unit 的 Handler。关闭时按相反顺序执行 `Shutdown`。普通资源由调用方管理生命周期。

## Recovery 与 OpenTelemetry

Runtime 默认启用 recovery、OpenTelemetry trace 和 metric。Weaver 使用 OpenTelemetry 的全局 `TracerProvider`、`MeterProvider` 和 `TextMapPropagator`，应用应在调用 `weaver.New` 前完成配置，并自行关闭 Provider 和 Exporter；未配置时 OpenTelemetry API 保持 no-op，Weaver 不会启动独立采集或导出进程。

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

Connect 生成的 Handler 会在同一路径上自动接受 Connect、gRPC 和 gRPC-Web。Runtime 不接管 `http.Server`，业务启动代码需要同时启用 HTTP/1、TLS HTTP/2 和明文 HTTP/2，才能兼容普通 HTTP 调用、HTTPS gRPC 和明文 h2c gRPC：

```go
protocols := new(http.Protocols)
protocols.SetHTTP1(true)
protocols.SetHTTP2(true)
protocols.SetUnencryptedHTTP2(true)

server := &http.Server{
    Addr:      ":8080",
    Handler:   runtime.Handler(),
    Protocols: protocols,
}
```

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
