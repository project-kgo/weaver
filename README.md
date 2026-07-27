# Weaver

Weaver 是一个很薄的 ConnectRPC 部署感知运行时。它让同一份 Go 代码、同一个二进制可以按配置运行成单体或多个部署单元，同时让业务代码保持不变。

核心原则只有三条：

1. protobuf service 是组件边界，unit 是部署与故障边界。
2. 同 unit 直接调用 Go 实现，跨 unit 使用 ConnectRPC。
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

组件创建顺序为：严格校验全部组件配置、创建全部本地实例、注入 `WithConfig`/`Resource`/`Ref`、按依赖顺序执行 `Init`、挂载当前 unit 的 Handler。关闭时按相反顺序执行 `Shutdown`。普通资源由调用方管理生命周期。

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

两种部署都会返回 `{"value":"echo:HELLO"}`。

## v0.1 边界

- 仅支持 unary RPC；生成器遇到 Streaming 会失败。
- placement 在启动后不可变，调整部署需要滚动重启。
- `Resource[T]` 按精确 Go 类型匹配，不支持命名资源和 Provider 图。
- 组件依赖必须是无环图。
- Connect interceptor 只处理传输层。业务校验、鉴权规则和领域错误不能只放在远程 Handler interceptor 中。
- Handler 不得修改 request；组件边界始终按“可能经过网络”设计。
