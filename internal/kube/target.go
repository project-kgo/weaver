package kube

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

type transportKind uint8

const (
	transportH2C transportKind = iota
	transportHTTPS
)

type target struct {
	namespace string
	service   string
	port      string
	portValue int
	transport transportKind
}

func (t target) key() string {
	return t.namespace + "/" + t.service + ":" + t.port + "/" + strconv.Itoa(int(t.transport))
}

func (t target) serviceKey() string {
	return t.namespace + "/" + t.service
}

func (t target) baseURL() string {
	scheme := "http"
	if t.transport == transportHTTPS {
		scheme = "https"
	}
	return scheme + "://" + t.serverName()
}

func (t target) serverName() string {
	return t.service + "." + t.namespace + ".svc"
}

func parseTarget(raw string) (target, error) {
	parsed, err := url.Parse(raw)
	if err != nil {
		return target{}, fmt.Errorf("kube: 无效 target %q: %w", raw, err)
	}
	if parsed.Scheme != "kube" || parsed.User != nil || parsed.Host == "" {
		return target{}, fmt.Errorf("kube: target 必须使用 kube://<namespace>/<service>:<port> 格式: %q", raw)
	}
	if parsed.RawFragment != "" || parsed.Fragment != "" {
		return target{}, fmt.Errorf("kube: target 不允许 fragment: %q", raw)
	}

	namespace := parsed.Host
	servicePort := strings.TrimPrefix(parsed.EscapedPath(), "/")
	if servicePort == "" || strings.Contains(servicePort, "/") {
		return target{}, fmt.Errorf("kube: target 必须包含 service 和 port: %q", raw)
	}
	servicePort, err = url.PathUnescape(servicePort)
	if err != nil {
		return target{}, fmt.Errorf("kube: target service 无效: %q", raw)
	}
	colon := strings.LastIndexByte(servicePort, ':')
	if colon <= 0 || colon == len(servicePort)-1 {
		return target{}, fmt.Errorf("kube: target 必须包含端口: %q", raw)
	}
	service, port := servicePort[:colon], servicePort[colon+1:]
	if !isDNSLabel(namespace) || !isDNSLabel(service) {
		return target{}, fmt.Errorf("kube: namespace 和 service 必须是有效的 DNS label: %q", raw)
	}

	result := target{namespace: namespace, service: service, port: port}
	if numeric, numericErr := strconv.Atoi(port); numericErr == nil {
		if numeric < 1 || numeric > 65535 {
			return target{}, fmt.Errorf("kube: 数字端口必须位于 1..65535: %q", raw)
		}
		result.portValue = numeric
	} else if !isDNSLabel(port) {
		return target{}, fmt.Errorf("kube: 端口名必须是有效的 DNS label: %q", raw)
	}

	query := parsed.Query()
	if len(query) > 1 || (len(query) == 1 && !query.Has("transport")) || len(query["transport"]) > 1 {
		return target{}, fmt.Errorf("kube: target 只支持 transport 查询参数: %q", raw)
	}
	switch query.Get("transport") {
	case "", "h2c":
		result.transport = transportH2C
	case "https":
		result.transport = transportHTTPS
	default:
		return target{}, fmt.Errorf("kube: transport 只支持 h2c 或 https: %q", raw)
	}
	return result, nil
}

func isDNSLabel(value string) bool {
	if value == "" || len(value) > 63 || value[0] == '-' || value[len(value)-1] == '-' {
		return false
	}
	for _, char := range value {
		if (char >= 'a' && char <= 'z') || (char >= '0' && char <= '9') || char == '-' {
			continue
		}
		return false
	}
	return true
}
