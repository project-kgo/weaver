// Package weaver 提供基于 ConnectRPC 的部署感知组件运行时。
//
// 业务代码始终依赖生成的组件接口；Runtime 根据 placement 配置选择
// 同进程本地代理或基于 HTTP/2 的跨进程 Connect client。
package weaver
