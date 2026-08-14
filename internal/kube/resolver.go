package kube

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	serviceAccountToken = "/var/run/secrets/kubernetes.io/serviceaccount/token"
	serviceAccountCA    = "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt"
	listPageSize        = 500
	defaultListTimeout  = 30 * time.Second
	defaultWatchTimeout = 5 * time.Minute
	watchDeadlineGrace  = 30 * time.Second
)

var errResourceVersionExpired = errors.New("kube: resourceVersion 已过期")

type sliceState struct {
	service     string
	name        string
	addressType string
	ports       []endpointPort
	endpoints   []endpoint
}

type namespaceWatch struct {
	namespace       string
	services        map[string]struct{}
	selector        string
	resourceVersion string
}

// Resolver 按 namespace 合并目标 Service，每个 namespace 只维护一套过滤后的 LIST/WATCH。
type Resolver struct {
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	apiBase   string
	apiClient *http.Client
	tokenFile string

	listTimeout  time.Duration
	watchTimeout time.Duration
	watchGrace   time.Duration

	startOnce sync.Once
	startErr  error

	mu             sync.Mutex
	namespaces     map[string]*namespaceWatch
	slices         map[string]map[string]sliceState
	sliceServices  map[string]string
	clients        map[string]*backendClient
	serviceClients map[string]map[string]*backendClient
	started        bool
	closed         bool
}

// NewInCluster 创建集群内 Resolver。调用 Start 前不会访问 Kubernetes API。
func NewInCluster() (*Resolver, error) {
	host := strings.TrimSpace(os.Getenv("KUBERNETES_SERVICE_HOST"))
	port := strings.TrimSpace(os.Getenv("KUBERNETES_SERVICE_PORT"))
	if host == "" || port == "" {
		return nil, fmt.Errorf("kube: 缺少 KUBERNETES_SERVICE_HOST 或 KUBERNETES_SERVICE_PORT")
	}
	ca, err := os.ReadFile(serviceAccountCA)
	if err != nil {
		return nil, fmt.Errorf("kube: 读取 ServiceAccount CA 失败: %w", err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(ca) {
		return nil, fmt.Errorf("kube: ServiceAccount CA 不包含有效证书")
	}

	transport := newAPITransport(roots)
	return newResolver(
		"https://"+net.JoinHostPort(host, port),
		&http.Client{Transport: transport},
		serviceAccountToken,
	), nil
}

func newAPITransport(roots *x509.CertPool) *http.Transport {
	protocols := new(http.Protocols)
	protocols.SetHTTP1(true)
	protocols.SetHTTP2(true)

	transport := http.DefaultTransport.(*http.Transport).Clone()
	// 主动协商 HTTP/2，让多个 namespace 的 WATCH 尽量复用同一条连接；
	// 不限制 MaxConnsPerHost，避免 API Server 回退 HTTP/1.1 时长期 WATCH 相互阻塞。
	transport.Proxy = nil
	transport.Protocols = protocols
	transport.ForceAttemptHTTP2 = true
	transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots}
	return transport
}

func newResolver(apiBase string, client *http.Client, tokenFile string) *Resolver {
	ctx, cancel := context.WithCancel(context.Background())
	return &Resolver{
		ctx:            ctx,
		cancel:         cancel,
		apiBase:        strings.TrimRight(apiBase, "/"),
		apiClient:      client,
		tokenFile:      tokenFile,
		listTimeout:    defaultListTimeout,
		watchTimeout:   defaultWatchTimeout,
		watchGrace:     watchDeadlineGrace,
		namespaces:     make(map[string]*namespaceWatch),
		slices:         make(map[string]map[string]sliceState),
		sliceServices:  make(map[string]string),
		clients:        make(map[string]*backendClient),
		serviceClients: make(map[string]map[string]*backendClient),
	}
}

// Resolve 注册并返回一个 target。新 Service 只能在 Start 前注册，保证 WATCH 过滤集合固定。
func (r *Resolver) Resolve(_ context.Context, rawTarget string) (string, *backendClient, error) {
	value, err := parseTarget(rawTarget)
	if err != nil {
		return "", nil, err
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return "", nil, fmt.Errorf("kube: Resolver 已关闭")
	}
	watch := r.namespaces[value.namespace]
	if watch == nil {
		if r.started {
			return "", nil, fmt.Errorf("kube: namespace %q 未在启动前注册", value.namespace)
		}
		watch = &namespaceWatch{namespace: value.namespace, services: make(map[string]struct{})}
		r.namespaces[value.namespace] = watch
	}
	if _, registered := watch.services[value.service]; !registered {
		if r.started {
			return "", nil, fmt.Errorf("kube: service %s/%s 未在启动前注册", value.namespace, value.service)
		}
		watch.services[value.service] = struct{}{}
	}

	client := r.clients[value.key()]
	if client == nil {
		client, err = newBackendClient(value)
		if err != nil {
			return "", nil, fmt.Errorf("kube: 创建 service %s/%s 负载均衡客户端失败: %w", value.namespace, value.service, err)
		}
		if err := client.update(r.aggregateLocked(value)); err != nil {
			client.close()
			return "", nil, fmt.Errorf("kube: 发布 service %s/%s endpoint 失败: %w", value.namespace, value.service, err)
		}
		r.clients[value.key()] = client
		serviceClients := r.serviceClients[value.serviceKey()]
		if serviceClients == nil {
			serviceClients = make(map[string]*backendClient)
			r.serviceClients[value.serviceKey()] = serviceClients
		}
		serviceClients[value.key()] = client
	}
	return value.baseURL(), client, nil
}

// Start 完成所有 namespace 的初始 LIST，随后分别启动长期 WATCH。
func (r *Resolver) Start(ctx context.Context) error {
	if ctx == nil {
		return fmt.Errorf("kube: context 不能为空")
	}
	r.startOnce.Do(func() {
		r.startErr = r.start(ctx)
	})
	return r.startErr
}

func (r *Resolver) start(ctx context.Context) error {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return fmt.Errorf("kube: Resolver 已关闭")
	}
	r.started = true
	watches := make([]*namespaceWatch, 0, len(r.namespaces))
	for _, watch := range r.namespaces {
		watch.selector = serviceSelector(watch.services)
		watches = append(watches, watch)
	}
	sort.Slice(watches, func(i, j int) bool { return watches[i].namespace < watches[j].namespace })
	r.mu.Unlock()
	if len(watches) == 0 {
		return nil
	}

	// 多 namespace 的初始 LIST 并行执行；HTTP/2 可在同一连接上复用这些请求。
	listCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	errorsByNamespace := make(chan error, len(watches))
	var lists sync.WaitGroup
	for _, watch := range watches {
		lists.Add(1)
		go func() {
			defer lists.Done()
			if err := r.relist(listCtx, watch); err != nil {
				errorsByNamespace <- err
				cancel()
			}
		}()
	}
	lists.Wait()
	close(errorsByNamespace)
	for err := range errorsByNamespace {
		if err != nil {
			return err
		}
	}

	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return fmt.Errorf("kube: Resolver 已关闭")
	}
	r.wg.Add(len(watches))
	r.mu.Unlock()
	for _, watch := range watches {
		go r.watchLoop(watch)
	}
	return nil
}

func (r *Resolver) relist(ctx context.Context, watch *namespaceWatch) error {
	requestCtx, cancel := context.WithTimeout(ctx, r.listTimeout)
	stop := context.AfterFunc(r.ctx, cancel)
	defer func() {
		stop()
		cancel()
	}()

	nextSlices := make(map[string]map[string]sliceState)
	continueToken := ""
	resourceVersion := ""
	for {
		endpoint, err := url.Parse(r.endpointSlicesURL(watch.namespace))
		if err != nil {
			return err
		}
		query := endpoint.Query()
		query.Set("labelSelector", watch.selector)
		query.Set("limit", strconv.Itoa(listPageSize))
		if continueToken != "" {
			query.Set("continue", continueToken)
		}
		endpoint.RawQuery = query.Encode()

		var page endpointSliceList
		if err := r.getJSON(requestCtx, endpoint.String(), &page); err != nil {
			return fmt.Errorf("kube: LIST namespace %q EndpointSlice 失败: %w", watch.namespace, err)
		}
		if resourceVersion == "" {
			resourceVersion = page.Metadata.ResourceVersion
		}
		for _, item := range page.Items {
			if r.interested(watch, item) {
				putSlice(nextSlices, compactSlice(item))
			}
		}
		continueToken = page.Metadata.Continue
		if continueToken == "" {
			break
		}
	}
	if resourceVersion == "" {
		return fmt.Errorf("kube: LIST namespace %q EndpointSlice 未返回 resourceVersion", watch.namespace)
	}

	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return fmt.Errorf("kube: Resolver 已关闭")
	}
	for service := range watch.services {
		delete(r.slices, watch.namespace+"/"+service)
	}
	for objectKey, serviceKey := range r.sliceServices {
		if strings.HasPrefix(objectKey, watch.namespace+"/") {
			delete(r.sliceServices, objectKey)
			delete(r.slices, serviceKey)
		}
	}
	for serviceKey, serviceSlices := range nextSlices {
		r.slices[serviceKey] = serviceSlices
		for sliceName := range serviceSlices {
			r.sliceServices[watch.namespace+"/"+sliceName] = serviceKey
		}
	}
	watch.resourceVersion = resourceVersion
	updates := r.namespaceUpdatesLocked(watch)
	r.mu.Unlock()
	return publishUpdates(updates)
}

func (r *Resolver) getJSON(ctx context.Context, endpoint string, destination any) error {
	request, err := r.newRequest(ctx, endpoint)
	if err != nil {
		return err
	}
	response, err := r.apiClient.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return responseError(response)
	}
	return json.NewDecoder(response.Body).Decode(destination)
}

func (r *Resolver) newRequest(ctx context.Context, endpoint string) (*http.Request, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Accept", "application/json")
	if r.tokenFile != "" {
		token, readErr := os.ReadFile(r.tokenFile)
		if readErr != nil {
			return nil, fmt.Errorf("读取 ServiceAccount token 失败: %w", readErr)
		}
		request.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(token)))
	}
	return request, nil
}

func (r *Resolver) watchLoop(watch *namespaceWatch) {
	defer r.wg.Done()
	backoff := 100 * time.Millisecond
	for {
		started := time.Now()
		err := r.watchOnce(watch)
		if r.ctx.Err() != nil {
			return
		}
		if errors.Is(err, errResourceVersionExpired) {
			if relistErr := r.relist(r.ctx, watch); relistErr == nil {
				backoff = 100 * time.Millisecond
				continue
			} else {
				err = relistErr
			}
		}
		if time.Since(started) >= r.watchTimeout/2 {
			backoff = 100 * time.Millisecond
		}
		slog.Warn("kube EndpointSlice watch 中断，将自动重连", "namespace", watch.namespace, "error", err, "backoff", backoff)
		delay := jitter(backoff)
		timer := time.NewTimer(delay)
		select {
		case <-r.ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		if backoff < 30*time.Second {
			backoff *= 2
			if backoff > 30*time.Second {
				backoff = 30 * time.Second
			}
		}
	}
}

func (r *Resolver) watchOnce(watch *namespaceWatch) error {
	r.mu.Lock()
	resourceVersion := watch.resourceVersion
	closed := r.closed
	r.mu.Unlock()
	if closed {
		return context.Canceled
	}

	endpoint, err := url.Parse(r.endpointSlicesURL(watch.namespace))
	if err != nil {
		return err
	}
	query := endpoint.Query()
	query.Set("labelSelector", watch.selector)
	query.Set("watch", "true")
	query.Set("resourceVersion", resourceVersion)
	query.Set("allowWatchBookmarks", "true")
	query.Set("timeoutSeconds", strconv.Itoa(timeoutSeconds(r.watchTimeout)))
	endpoint.RawQuery = query.Encode()

	// 服务端 timeoutSeconds 只是提示；客户端 deadline 负责回收半断开的 WATCH 并触发重连。
	requestCtx, cancel := context.WithTimeout(r.ctx, r.watchTimeout+r.watchGrace)
	defer cancel()
	request, err := r.newRequest(requestCtx, endpoint.String())
	if err != nil {
		return err
	}
	response, err := r.apiClient.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusGone {
		return errResourceVersionExpired
	}
	if response.StatusCode != http.StatusOK {
		return responseError(response)
	}

	decoder := json.NewDecoder(response.Body)
	for {
		var event watchEvent
		if err := decoder.Decode(&event); err != nil {
			return err
		}
		switch event.Type {
		case "ADDED", "MODIFIED", "DELETED":
			var item endpointSlice
			if err := json.Unmarshal(event.Object, &item); err != nil {
				return fmt.Errorf("kube: 解码 EndpointSlice watch 事件失败: %w", err)
			}
			r.applyEvent(watch, event.Type, item)
		case "BOOKMARK":
			var bookmark struct {
				Metadata objectMetadata `json:"metadata"`
			}
			if err := json.Unmarshal(event.Object, &bookmark); err != nil {
				return fmt.Errorf("kube: 解码 watch bookmark 失败: %w", err)
			}
			r.updateResourceVersion(watch, bookmark.Metadata.ResourceVersion)
		case "ERROR":
			var status statusObject
			if err := json.Unmarshal(event.Object, &status); err != nil {
				return fmt.Errorf("kube: 解码 watch 错误失败: %w", err)
			}
			if status.Code == http.StatusGone || status.Reason == "Expired" {
				return errResourceVersionExpired
			}
			return fmt.Errorf("kube: watch 错误: %s", status.Message)
		default:
			return fmt.Errorf("kube: 未知 watch 事件类型 %q", event.Type)
		}
	}
}

func (r *Resolver) applyEvent(watch *namespaceWatch, eventType string, item endpointSlice) {
	if item.Metadata.Name == "" || item.Metadata.Namespace != watch.namespace {
		return
	}
	objectKey := watch.namespace + "/" + item.Metadata.Name
	newService := serviceName(item)
	newServiceKey := watch.namespace + "/" + newService

	r.mu.Lock()
	previousServiceKey := r.sliceServices[objectKey]
	affected := make(map[string]struct{}, 2)
	if previousServiceKey != "" {
		affected[previousServiceKey] = struct{}{}
	}
	_, interested := watch.services[newService]
	if eventType == "DELETED" || !interested {
		r.deleteSliceLocked(previousServiceKey, item.Metadata.Name)
		delete(r.sliceServices, objectKey)
	} else {
		if previousServiceKey != "" && previousServiceKey != newServiceKey {
			r.deleteSliceLocked(previousServiceKey, item.Metadata.Name)
		}
		putSlice(r.slices, compactSlice(item))
		r.sliceServices[objectKey] = newServiceKey
		affected[newServiceKey] = struct{}{}
	}
	if item.Metadata.ResourceVersion != "" {
		watch.resourceVersion = item.Metadata.ResourceVersion
	}
	updates := r.servicesUpdatesLocked(affected)
	r.mu.Unlock()
	if err := publishUpdates(updates); err != nil {
		slog.Error("kube 发布 endpoint 更新失败", "namespace", watch.namespace, "error", err)
	}
}

func (r *Resolver) deleteSliceLocked(serviceKey, sliceName string) {
	if serviceKey == "" {
		return
	}
	if serviceSlices := r.slices[serviceKey]; serviceSlices != nil {
		delete(serviceSlices, sliceName)
		if len(serviceSlices) == 0 {
			delete(r.slices, serviceKey)
		}
	}
}

func (r *Resolver) updateResourceVersion(watch *namespaceWatch, value string) {
	if value == "" {
		return
	}
	r.mu.Lock()
	watch.resourceVersion = value
	r.mu.Unlock()
}

type clientUpdate struct {
	client    *backendClient
	addresses []string
}

func (r *Resolver) namespaceUpdatesLocked(watch *namespaceWatch) []clientUpdate {
	services := make(map[string]struct{}, len(watch.services))
	for service := range watch.services {
		services[watch.namespace+"/"+service] = struct{}{}
	}
	return r.servicesUpdatesLocked(services)
}

func (r *Resolver) servicesUpdatesLocked(services map[string]struct{}) []clientUpdate {
	var updates []clientUpdate
	for service := range services {
		for _, client := range r.serviceClients[service] {
			updates = append(updates, clientUpdate{client: client, addresses: r.aggregateLocked(client.target)})
		}
	}
	return updates
}

func publishUpdates(updates []clientUpdate) error {
	var result error
	for _, update := range updates {
		if err := update.client.update(update.addresses); err != nil {
			result = errors.Join(result, err)
		}
	}
	return result
}

func (r *Resolver) aggregateLocked(value target) []string {
	unique := make(map[string]struct{})
	for _, slice := range r.slices[value.serviceKey()] {
		if slice.addressType != "IPv4" && slice.addressType != "IPv6" {
			continue
		}
		port, available := targetPort(slice, value)
		if !available {
			continue
		}
		for _, endpoint := range slice.endpoints {
			if endpoint.Conditions.Ready != nil && !*endpoint.Conditions.Ready {
				continue
			}
			for _, rawAddress := range endpoint.Addresses {
				address, err := netip.ParseAddr(rawAddress)
				if err != nil || (slice.addressType == "IPv4") != address.Is4() {
					continue
				}
				unique[net.JoinHostPort(address.String(), strconv.Itoa(port))] = struct{}{}
			}
		}
	}
	addresses := make([]string, 0, len(unique))
	for address := range unique {
		addresses = append(addresses, address)
	}
	sort.Strings(addresses)
	return addresses
}

func targetPort(slice sliceState, value target) (int, bool) {
	if value.portValue != 0 {
		for _, port := range slice.ports {
			if isTCP(port.Protocol) {
				return value.portValue, true
			}
		}
		return 0, false
	}
	for _, port := range slice.ports {
		if port.Name != nil && *port.Name == value.port && port.Port != nil && isTCP(port.Protocol) {
			return *port.Port, true
		}
	}
	return 0, false
}

func isTCP(protocol *string) bool {
	return protocol == nil || *protocol == "" || *protocol == "TCP"
}

func compactSlice(item endpointSlice) sliceState {
	return sliceState{
		service:     item.Metadata.Namespace + "/" + serviceName(item),
		name:        item.Metadata.Name,
		addressType: item.AddressType,
		ports:       item.Ports,
		endpoints:   item.Endpoints,
	}
}

func putSlice(destination map[string]map[string]sliceState, item sliceState) {
	if strings.HasPrefix(item.service, "/") || strings.HasSuffix(item.service, "/") || item.name == "" {
		return
	}
	serviceSlices := destination[item.service]
	if serviceSlices == nil {
		serviceSlices = make(map[string]sliceState)
		destination[item.service] = serviceSlices
	}
	serviceSlices[item.name] = item
}

func serviceName(item endpointSlice) string {
	return item.Metadata.Labels["kubernetes.io/service-name"]
}

func serviceSelector(services map[string]struct{}) string {
	names := make([]string, 0, len(services))
	for service := range services {
		names = append(names, service)
	}
	sort.Strings(names)
	return "kubernetes.io/service-name in (" + strings.Join(names, ",") + ")"
}

func (r *Resolver) interested(watch *namespaceWatch, item endpointSlice) bool {
	if item.Metadata.Namespace != watch.namespace || item.Metadata.Name == "" {
		return false
	}
	_, ok := watch.services[serviceName(item)]
	return ok
}

func (r *Resolver) endpointSlicesURL(namespace string) string {
	return r.apiBase + "/apis/discovery.k8s.io/v1/namespaces/" + url.PathEscape(namespace) + "/endpointslices"
}

func responseError(response *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(response.Body, 64<<10))
	message := strings.TrimSpace(string(body))
	if message == "" {
		message = response.Status
	}
	if response.StatusCode == http.StatusGone {
		return errResourceVersionExpired
	}
	return fmt.Errorf("Kubernetes API 返回 %s: %s", response.Status, message)
}

func timeoutSeconds(value time.Duration) int {
	seconds := int((value + time.Second - 1) / time.Second)
	if seconds < 1 {
		return 1
	}
	return seconds
}

func jitter(value time.Duration) time.Duration {
	if value <= 0 {
		return 0
	}
	spread := value / 5
	return value - spread + time.Duration(rand.Int64N(int64(spread*2)+1))
}

// Shutdown 停止全部 namespace WATCH，并关闭 Kubernetes 与 Pod 空闲连接。
func (r *Resolver) Shutdown(context.Context) error {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil
	}
	r.closed = true
	clients := make([]*backendClient, 0, len(r.clients))
	for _, client := range r.clients {
		clients = append(clients, client)
	}
	r.clients = make(map[string]*backendClient)
	r.serviceClients = make(map[string]map[string]*backendClient)
	r.mu.Unlock()

	r.cancel()
	r.wg.Wait()
	for _, client := range clients {
		client.close()
	}
	r.apiClient.CloseIdleConnections()
	return nil
}
