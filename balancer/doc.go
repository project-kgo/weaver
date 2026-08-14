// Package balancer 提供面向动态 endpoint 集合的 HTTP 负载均衡客户端。
//
// Client 使用 P2C 从两个随机候选中选择综合得分更低的 endpoint，
// 并通过延迟、错误率和在途请求数动态调整选择。
package balancer
