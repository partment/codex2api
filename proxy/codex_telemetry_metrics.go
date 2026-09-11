package proxy

import (
	"encoding/json"
	"log"
	"sort"
	"strconv"
	"strings"
	"time"
)

type codexMetricDescriptor struct {
	name       string
	kind       string
	unit       string
	attributes string
}

// codexMetricPoint 是单个 (指标, 属性集合) 在一个导出周期内的聚合状态。
//
// 真实 Codex 客户端走 OpenTelemetry SDK 的 PeriodicReader + Delta 时间性：
// SDK 会对每个 (instrument, attribute set) 维护 count/sum/min/max/bucketCounts，
// 而不是把多轮观测压成一个标量。这里照搬同一模型，保证不同 model/session 的
// 观测各自成点、直方图分布不丢失。
type codexMetricPoint struct {
	descriptor codexMetricDescriptor
	profile    codexTelemetryProfile
	attributes map[string]string
	// sum 指标（counter）：周期内累加值。
	sumValue float64
	// histogram：周期内逐观测累加的聚合量。
	count        uint64
	sum          float64
	min          float64
	max          float64
	bucketCounts []uint64
	// gauge：周期内最后一次观测值。
	lastValue float64
	observed  bool
}

type codexMetricState struct {
	profile           codexTelemetryProfile
	started           time.Time
	lastSeen          time.Time
	points            map[string]*codexMetricPoint
	externalAgentSent bool
}

var codexMetricDescriptors = []codexMetricDescriptor{
	{"codex.process.start", "sum", "", "originator"},
	{"codex.sqlite.init.count", "sum", "", "db,error,originator,phase,status"},
	{"codex.sqlite.init.duration_ms", "histogram", "ms", "db,error,originator,phase,status"},
	{"codex.app_server.codex_home.size_bytes", "histogram", "", "compression_enabled,directory"},
	{"codex.remote_models.fetch_update.duration_ms", "histogram", "ms", ""},
	{"codex.plugins.loaded_cache.event", "sum", "", "event"},
	{"codex.remote_models.load_cache.duration_ms", "histogram", "ms", ""},
	{"codex.plugins.loaded_cache.wait.duration_ms", "histogram", "ms", ""},
	{"codex.plugins.loaded_cache.load.duration_ms", "histogram", "ms", ""},
	{"codex.plugins.loaded_cache.request", "sum", "", "outcome"},
	{"codex.mcp.protocol_discovery", "sum", "", "mode,outcome,server_kind"},
	{"codex.mcp.protocol_discovery.duration_ms", "histogram", "ms", "mode,outcome,server_kind"},
	{"codex.mcp.tools.fetch_uncached.duration_ms", "histogram", "ms", "trigger"},
	{"codex.mcp.tools.list.duration_ms", "histogram", "ms", "cache"},
	{"codex.apps.installed.duration_ms", "histogram", "ms", "force_refresh,outcome,path,refresh,reload,retained_previous_snapshot"},
	{"codex.apps.installed.response_bytes", "histogram", "", "path"},
	{"codex.apps.installed.connector_count", "histogram", "", "path"},
	{"codex.apps.installed.tool_count", "histogram", "", "path"},
	{"codex.apps.snapshot.age_ms", "histogram", "ms", "observation,path"},
	{"codex.apps.read.duration_ms", "histogram", "ms", "include_tools"},
	{"codex.sqlite.logs.write.count", "sum", "", "error,originator,status"},
	{"codex.sqlite.logs.write.duration_ms", "histogram", "ms", "error,originator,status"},
	{"codex.sqlite.logs.write.bytes", "histogram", "", "error,originator,status"},
	{"codex.sqlite.logs.write.entries", "histogram", "", "error,originator,status"},
	{"codex.sqlite.logs.write.max_entry_bytes", "histogram", "", "error,originator,status"},
	{"codex.mcp.tools.cache_write.duration_ms", "histogram", "ms", "status"},
	{"codex.mcp.tools.cache_publish.duration_ms", "histogram", "ms", "result,source"},
	{"codex.apps.refresh.duration_ms", "histogram", "ms", "path,trigger"},
	{"codex.feature.state", "sum", "", "app.version,auth_mode,feature,model,originator,service_name,session_source,value"},
	{"codex.thread.started", "sum", "", "app.version,auth_mode,is_git,model,originator,service_name,session_source"},
	{"codex.shell_snapshot.duration_ms", "histogram", "ms", "app.version,auth_mode,model,originator,session_source,success,version"},
	{"codex.shell_snapshot", "sum", "", "app.version,auth_mode,failure_reason,model,originator,session_source,success,version"},
	{"codex.startup.phase.duration_ms", "histogram", "ms", "app.version,auth_mode,model,originator,phase,service_name,session_source,status"},
	{"codex.websocket.request", "sum", "", "app.version,auth_mode,model,originator,service_name,session_source,success"},
	{"codex.websocket.request.duration_ms", "histogram", "ms", "app.version,auth_mode,model,originator,service_name,session_source,success"},
	{"codex.rollout_compression.materialize", "sum", "", "outcome"},
	{"codex.websocket.event", "sum", "", "app.version,auth_mode,kind,model,originator,service_name,session_source,success"},
	{"codex.websocket.event.duration_ms", "histogram", "ms", "app.version,auth_mode,kind,model,originator,service_name,session_source,success"},
	{"codex.startup_prewarm.duration_ms", "histogram", "ms", "app.version,auth_mode,model,originator,service_name,session_source,status"},
	{"codex.startup_prewarm.age_at_first_turn_ms", "histogram", "ms", "app.version,auth_mode,model,originator,service_name,session_source,status"},
	{"codex.thread.skills.enabled_total", "histogram", "", "app.version,auth_mode,catalog_surface,model,originator,service_name,session_source"},
	{"codex.thread.skills.kept_total", "histogram", "", "app.version,auth_mode,catalog_surface,model,originator,service_name,session_source"},
	{"codex.thread.skills.truncated", "histogram", "", "app.version,auth_mode,catalog_surface,model,originator,service_name,session_source"},
	{"codex.thread.skills.description_truncated_chars", "histogram", "", "app.version,auth_mode,catalog_surface,model,originator,service_name,session_source"},
	{"codex.skills.shadow_selection", "sum", "", "candidate_set_truncated,method,query_script,query_truncated,status"},
	{"codex.skills.shadow_selection.duration_ms", "histogram", "ms", "candidate_set_truncated,method,query_script,query_truncated,status"},
	{"codex.skills.shadow_selection.catalog_entries", "histogram", "", "candidate_set_truncated,method,query_script,query_truncated,status"},
	{"codex.skills.shadow_selection.selected_entries", "histogram", "", "candidate_set_truncated,method,query_script,query_truncated,status"},
	{"codex.skills.shadow_selection.query_terms", "histogram", "", "candidate_set_truncated,method,query_script,query_truncated,status"},
	{"codex.skills.shadow_selection.reduction_bps", "histogram", "", "candidate_set_truncated,method,query_script,query_truncated,status"},
	{"codex.turn.ttft.duration_ms", "histogram", "ms", "app.version,auth_mode,model,originator,service_name,session_source"},
	{"codex.turn.ttfm.duration_ms", "histogram", "ms", "app.version,auth_mode,model,originator,service_name,session_source"},
	{"codex.responses_api_overhead.duration_ms", "histogram", "ms", "app.version,auth_mode,model,originator,service_name,session_source"},
	{"codex.responses_api_inference_time.duration_ms", "histogram", "ms", "app.version,auth_mode,model,originator,service_name,session_source"},
	{"codex.responses_api_engine_iapi_tbt.duration_ms", "histogram", "ms", "app.version,auth_mode,model,originator,service_name,session_source"},
	{"codex.turn.e2e_duration_ms", "histogram", "ms", "app.version,auth_mode,model,originator,service_name,session_source"},
	{"codex.turn.network_proxy", "sum", "", "active,app.version,auth_mode,model,originator,service_name,session_source,tmp_mem_enabled"},
	{"codex.turn.tool.call", "histogram", "", "app.version,auth_mode,model,originator,service_name,session_source,tmp_mem_enabled"},
	{"codex.turn.memory", "sum", "", "app.version,auth_mode,config_use_memories,feature_enabled,has_citations,model,originator,read_allowed,service_name,session_source"},
	{"codex.turn.unified_exec.running_processes", "sum", "", "app.version,auth_mode,model,originator,service_name,session_source"},
	{"codex.windows_mxc.available", "sum", "", "available"},
	{"codex.tool.unified_exec", "sum", "", "app.version,auth_mode,model,originator,session_source,tty"},
	{"codex.hooks.run", "sum", "", "app.version,auth_mode,execution_mode,handler_type,hook_name,model,originator,session_source,source,status"},
	{"codex.hooks.run.duration_ms", "histogram", "ms", "app.version,auth_mode,execution_mode,handler_type,hook_name,model,originator,session_source,source,status"},
	{"codex.external_agent_config.detect", "sum", "", "migration_type"},
	{"codex.rollout.size_bytes", "histogram", "", ""},
}

// codexMetricDescriptorIndex 按名称索引描述符，供记录路径按名字取类型/属性/单位。
var codexMetricDescriptorIndex = func() map[string]codexMetricDescriptor {
	index := make(map[string]codexMetricDescriptor, len(codexMetricDescriptors))
	for _, descriptor := range codexMetricDescriptors {
		index[descriptor.name] = descriptor
	}
	return index
}()

// codexDynamicMetricNames 是只随回合增量发送、不出现在启动批次的指标。
// 用显式名单替代 `codexMetricDescriptors[:62]` 的位置契约：增删或重排目录
// 都不会再静默改变启动/增量的划分。
var codexDynamicMetricNames = map[string]struct{}{
	"codex.hooks.run":                    {},
	"codex.hooks.run.duration_ms":        {},
	"codex.external_agent_config.detect": {},
	"codex.rollout.size_bytes":           {},
}

// codexStartupMetric 报告指标是否属于账号首次出现时发送的启动批次。
func codexStartupMetric(name string) bool {
	_, dynamic := codexDynamicMetricNames[name]
	return !dynamic
}

// codexStatsigDisabledMetrics 是真实 Codex 客户端明确不通过 Statsig 路由发送的
// 指标（codex-rs/otel/src/metrics/config.rs 的 STATSIG_DISABLED_METRICS）。当前
// 目录未包含这些名字，这里保留护栏，避免后续新增时误发。
var codexStatsigDisabledMetrics = map[string]struct{}{
	"codex.api_request":                                   {},
	"codex.api_request.duration_ms":                       {},
	"codex.conversation.turn.count":                       {},
	"exec_server_client_requests_total":                   {},
	"codex.responses_api_engine_iapi_ttft.duration_ms":    {},
	"codex.responses_api_engine_service_tbt.duration_ms":  {},
	"codex.responses_api_engine_service_ttft.duration_ms": {},
	"codex.tool.call":                                     {},
	"codex.tool.call.duration_ms":                         {},
	"codex.turn.cost_microusd":                            {},
	"codex.turn.token_usage":                              {},
}

// codexTelemetryStatsigAllowed 报告某指标是否允许走 Statsig。
func codexTelemetryStatsigAllowed(name string) bool {
	_, disabled := codexStatsigDisabledMetrics[name]
	return !disabled
}

// newCodexMetricPoint 用一次观测初始化聚合点。
func newCodexMetricPoint(profile codexTelemetryProfile, descriptor codexMetricDescriptor, attributes map[string]string, value float64) *codexMetricPoint {
	bounds := codexBoundsFor(descriptor)
	point := &codexMetricPoint{
		descriptor: descriptor, profile: profile, attributes: attributes, min: value, max: value,
		bucketCounts: make([]uint64, len(bounds)+1), observed: true,
	}
	switch descriptor.kind {
	case "sum":
		point.sumValue = value
	case "histogram":
		point.count, point.sum = 1, value
		point.bucketCounts[codexBucketIndex(bounds, value)] = 1
	default:
		point.lastValue = value
	}
	return point
}

// recordCodexMetricPoint 把一次观测并入聚合点。调用方需持有 manager 锁。
func recordCodexMetricPoint(state *codexMetricState, profile codexTelemetryProfile, name string, value float64) {
	descriptor, ok := codexMetricDescriptorIndex[name]
	if !ok {
		return
	}
	attributes := codexMetricAttributeMap(profile, descriptor)
	resource := codexResourceAttributes(profile)
	encodedResource, _ := json.Marshal(resource)
	key := name + "\x00" + string(encodedResource) + "\x00" + codexMetricAttributeSignature(attributes)
	point := state.points[key]
	if point == nil {
		state.points[key] = newCodexMetricPoint(profile, descriptor, attributes, value)
		return
	}
	switch descriptor.kind {
	case "sum":
		point.sumValue += value
	case "histogram":
		if value < point.min {
			point.min = value
		}
		if value > point.max {
			point.max = value
		}
		point.count++
		point.sum += value
		bounds := codexBoundsFor(descriptor)
		point.bucketCounts[codexBucketIndex(bounds, value)]++
	default:
		point.lastValue, point.observed = value, true
	}
}

// touchMetrics 初始化账号级指标状态并发送启动指标。
func (m *codexTelemetryManager) touchMetrics(profile codexTelemetryProfile) {
	m.mu.Lock()
	state, created := m.ensureMetricStateLocked(profile, profile.started)
	started := state.started
	m.mu.Unlock()
	if created {
		m.enqueueStartupMetrics(profile, started)
	}
}

// ensureMetricStateLocked 在持锁状态下取得或创建账号级指标状态。
// 创建时 lastSeen 取当前观测时间而非 profile.started：超过 TTL 的长回合结束后
// 若仍以回合开始时间登记，下一次 flush 会立刻把状态清掉。
func (m *codexTelemetryManager) ensureMetricStateLocked(profile codexTelemetryProfile, now time.Time) (*codexMetricState, bool) {
	accountID := profile.client.account.ID()
	if state := m.metrics[accountID]; state != nil {
		state.profile, state.lastSeen = profile, now
		return state, false
	}
	state := &codexMetricState{profile: profile, started: now, lastSeen: now, points: make(map[string]*codexMetricPoint)}
	m.metrics[accountID] = state
	return state, true
}

// enqueueStartupMetrics 发送账号首次出现时的启动指标批次。
func (m *codexTelemetryManager) enqueueStartupMetrics(profile codexTelemetryProfile, started time.Time) {
	timing := codexTelemetryTimingDebug()
	var startedAt time.Time
	if timing {
		startedAt = time.Now()
	}
	points := make([]*codexMetricPoint, 0, 62)
	for _, descriptor := range codexMetricDescriptors {
		if !codexStartupMetric(descriptor.name) {
			continue
		}
		attributes := codexMetricAttributeMap(profile, descriptor)
		points = append(points, newCodexMetricPoint(profile, descriptor, attributes, codexStartupMetricValue(profile, descriptor)))
	}
	m.enqueueMetrics(profile.client, buildCodexMetricsPayload(profile, started, points))
	if timing {
		log.Printf("[TELEMETRY-TIMING] startup_metrics account=%d points=%d build_ms=%d",
			profile.client.account.ID(), len(points), time.Since(startedAt).Milliseconds())
	}
}

// recordTurnMetrics 累积一次 turn 产生的增量指标。
//
// 状态查找与创建在同一次持锁内完成：此前先解锁调 touchMetrics 再重新加锁查表，
// 若分钟级 flush 恰好在中间按 TTL 清掉刚创建的状态，这里会对 nil 解引用。
func (m *codexTelemetryManager) recordTurnMetrics(profile codexTelemetryProfile, result codexTelemetryTerminal) {
	now := time.Now()
	m.mu.Lock()
	state, created := m.ensureMetricStateLocked(profile, now)
	started := state.started
	recordCodexMetricPoint(state, profile, "codex.turn.e2e_duration_ms", float64(max(now.Sub(profile.started).Milliseconds(), 0)))
	recordCodexMetricPoint(state, profile, "codex.turn.ttft.duration_ms", float64(elapsedMillis(profile.started, result.firstEvent, now)))
	recordCodexMetricPoint(state, profile, "codex.turn.ttfm.duration_ms", float64(elapsedMillis(profile.started, result.firstToken, now)))
	// 每轮 4 个 hook 执行：计数累加 4，时长为 4 个独立观测。
	recordCodexMetricPoint(state, profile, "codex.hooks.run", 4)
	for index := range 4 {
		duration := float64(50 + simulatedInt(profile.turnID+":hook:"+strconv.Itoa(index), 1500))
		recordCodexMetricPoint(state, profile, "codex.hooks.run.duration_ms", duration)
	}
	recordCodexMetricPoint(state, profile, "codex.turn.tool.call", float64(boolInt(profile.dynamicTool)+boolInt(profile.fileChange)))
	if profile.command {
		recordCodexMetricPoint(state, profile, "codex.tool.unified_exec", 1)
	}
	if profile.fileChange {
		recordCodexMetricPoint(state, profile, "codex.rollout.size_bytes", float64(1024+simulatedInt(profile.turnID+":rollout", 196608)))
	}
	if !state.externalAgentSent {
		recordCodexMetricPoint(state, profile, "codex.external_agent_config.detect", 1)
		state.externalAgentSent = true
	}
	m.mu.Unlock()
	if created {
		m.enqueueStartupMetrics(profile, started)
	}
}

// flushMetrics 发送待处理指标并清理过期账号和 thread 状态。
func (m *codexTelemetryManager) flushMetrics(now time.Time) {
	type batch struct {
		client  codexTelemetryClient
		profile codexTelemetryProfile
		started time.Time
		points  []*codexMetricPoint
	}
	m.mu.Lock()
	batches := make([]batch, 0, len(m.metrics))
	for accountID, state := range m.metrics {
		if len(state.points) > 0 {
			points := make([]*codexMetricPoint, 0, len(state.points))
			for _, point := range state.points {
				points = append(points, point)
			}
			batches = append(batches, batch{state.profile.client, state.profile, state.started, points})
			state.points = make(map[string]*codexMetricPoint)
		}
		if now.Sub(state.lastSeen) > codexTelemetryStateTTL {
			delete(m.metrics, accountID)
		}
	}
	for key, seen := range m.threads {
		if now.Sub(seen) > codexTelemetryStateTTL {
			delete(m.threads, key)
		}
	}
	m.mu.Unlock()
	for _, item := range batches {
		m.enqueueMetrics(item.client, buildCodexMetricsPayload(item.profile, item.started, item.points))
	}
}

// enqueueMetrics 将非空 OTLP payload 放入异步发送队列。
func (m *codexTelemetryManager) enqueueMetrics(client codexTelemetryClient, body []byte) {
	if len(body) > 0 {
		m.enqueue(codexTelemetryJob{client: client, url: codexMetricsEndpoint, body: body, metrics: true})
	}
}

// buildCodexMetricsPayload 将聚合点按完整 resource identity 分组后编码为 OTLP JSON。
func buildCodexMetricsPayload(fallbackProfile codexTelemetryProfile, started time.Time, points []*codexMetricPoint) []byte {
	type resourceGroup struct {
		attributes []any
		metrics    []any
	}
	groups := make(map[string]*resourceGroup)
	for _, point := range points {
		if !codexTelemetryStatsigAllowed(point.descriptor.name) {
			continue
		}
		profile := point.profile
		if profile.client.userAgent == "" {
			profile = fallbackProfile
		}
		attributes := codexResourceAttributes(profile)
		encoded, _ := json.Marshal(attributes)
		key := string(encoded)
		group := groups[key]
		if group == nil {
			group = &resourceGroup{attributes: attributes}
			groups[key] = group
		}
		group.metrics = append(group.metrics, codexOTLPMetricPoint(started, point))
	}

	keys := make([]string, 0, len(groups))
	for key := range groups {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	resourceMetrics := make([]any, 0, len(keys))
	for _, key := range keys {
		group := groups[key]
		resource := map[string]any{"attributes": group.attributes, "droppedAttributesCount": 0, "entityRefs": []any{}}
		scope := map[string]any{"name": "codex", "version": "", "attributes": []any{}, "droppedAttributesCount": 0}
		scopeMetrics := map[string]any{"scope": scope, "metrics": group.metrics, "schemaUrl": ""}
		resourceMetrics = append(resourceMetrics, map[string]any{"resource": resource, "scopeMetrics": []any{scopeMetrics}, "schemaUrl": ""})
	}
	body, _ := json.Marshal(map[string]any{"resourceMetrics": resourceMetrics})
	return body
}

// codexOTLPMetricPoint 将聚合点转换为 OTLP sum、histogram 或 gauge。
func codexOTLPMetricPoint(started time.Time, point *codexMetricPoint) map[string]any {
	nowNanos := strconv.FormatInt(time.Now().UnixNano(), 10)
	startNanos := strconv.FormatInt(started.UnixNano(), 10)
	dataPoint := map[string]any{
		"attributes": codexOTLPAttributes(point.attributes), "startTimeUnixNano": startNanos,
		"timeUnixNano": nowNanos, "exemplars": []any{}, "flags": 0,
	}
	metric := map[string]any{"name": point.descriptor.name, "description": "", "unit": point.descriptor.unit, "metadata": []any{}}
	switch point.descriptor.kind {
	case "sum":
		dataPoint["asInt"] = int64(point.sumValue)
		metric["sum"] = map[string]any{"dataPoints": []any{dataPoint}, "aggregationTemporality": 1, "isMonotonic": true}
	case "gauge":
		dataPoint["asInt"] = int64(point.lastValue)
		metric["gauge"] = map[string]any{"dataPoints": []any{dataPoint}}
	default:
		if point.descriptor.unit == "ms" {
			metric["description"] = "Duration in milliseconds."
		}
		dataPoint["count"], dataPoint["sum"], dataPoint["min"], dataPoint["max"] = point.count, point.sum, point.min, point.max
		dataPoint["explicitBounds"] = codexBoundsFor(point.descriptor)
		dataPoint["bucketCounts"] = point.bucketCounts
		metric["histogram"] = map[string]any{"dataPoints": []any{dataPoint}, "aggregationTemporality": 1}
	}
	return metric
}

// codexHistogramBounds 是 Codex duration 直方图使用的毫秒边界，与客户端
// MILLISECOND_DURATION_BOUNDARIES 完全一致。
var codexHistogramBounds = []float64{0, 5, 10, 25, 50, 75, 100, 250, 500, 750, 1000, 1250, 1500, 1750, 2000, 2250, 2500, 3000, 3500, 4000, 4500, 5000, 6000, 7000, 7500, 8000, 9000, 10000, 12000, 15000, 20000, 30000, 60000, 120000}

// codexSecondHistogramBounds 对应客户端 SECOND_DURATION_BOUNDARIES。
var codexSecondHistogramBounds = []float64{0, 0.005, 0.01, 0.025, 0.05, 0.075, 0.1, 0.25, 0.5, 0.75, 1.0, 2.5, 5.0, 7.5, 10.0, 12.0, 15.0, 20.0, 30.0, 60.0, 120.0}

// codexOtelDefaultHistogramBounds 是 OpenTelemetry SDK 对未指定边界的直方图
// 使用的默认桶。
var codexOtelDefaultHistogramBounds = []float64{0, 5, 10, 25, 50, 75, 100, 250, 500, 750, 1000, 2500, 5000, 7500, 10000}

// codexBoundsFor 按单位返回指标应使用的显式边界。真实客户端只给 duration
// 指标配置边界，其它直方图走 SDK 默认桶。
func codexBoundsFor(descriptor codexMetricDescriptor) []float64 {
	switch descriptor.unit {
	case "ms":
		return codexHistogramBounds
	case "s":
		return codexSecondHistogramBounds
	default:
		return codexOtelDefaultHistogramBounds
	}
}

// codexBucketIndex 返回观测值落入的桶下标（最后一个为 +Inf 桶）。
func codexBucketIndex(bounds []float64, value float64) int {
	for index, bound := range bounds {
		if value <= bound {
			return index
		}
	}
	return len(bounds)
}

// codexMetricAttributeMap 返回指标属性集合。
func codexMetricAttributeMap(profile codexTelemetryProfile, descriptor codexMetricDescriptor) map[string]string {
	values := make(map[string]string)
	if descriptor.attributes == "" {
		return values
	}
	for _, name := range strings.Split(descriptor.attributes, ",") {
		if value := codexMetricAttributeValue(profile, descriptor.name, name); value != "" {
			values[name] = value
		}
	}
	return values
}

// codexMetricAttributeSignature 生成稳定的属性集签名，用于把观测归并到同一个点。
func codexMetricAttributeSignature(attributes map[string]string) string {
	keys := make([]string, 0, len(attributes))
	for key := range attributes {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var builder strings.Builder
	for _, key := range keys {
		builder.WriteString(key)
		builder.WriteByte('=')
		builder.WriteString(attributes[key])
		builder.WriteByte('\x1f')
	}
	return builder.String()
}

// codexResourceAttributes 构造 OTLP resource 级客户端属性。
func codexResourceAttributes(profile codexTelemetryProfile) []any {
	_, _, osName, osVersion, _ := codexUserAgentParts(profile.client.userAgent, profile.client.version)
	return codexOTLPAttributes(map[string]string{
		"os": osName, "os_version": osVersion, "service.version": profile.client.version, "env": "dev",
		"telemetry.sdk.version": "0.31.0", "telemetry.sdk.language": "rust",
		"service.name": codexMetricResourceService(profile), "telemetry.sdk.name": "opentelemetry",
	})
}

// codexOTLPAttributes 按 key 排序编码 OTLP 字符串属性。
func codexOTLPAttributes(values map[string]string) []any {
	attributes := make([]any, 0, len(values))
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		value := values[key]
		attributes = append(attributes, map[string]any{"key": key, "value": map[string]any{"stringValue": value}})
	}
	return attributes
}

// codexMetricAttributeValue 返回指标属性的模拟值。
func codexMetricAttributeValue(profile codexTelemetryProfile, metric, name string) string {
	if name == "originator" && (metric == "codex.process.start" || strings.HasPrefix(metric, "codex.sqlite.")) {
		return codexMetricResourceService(profile)
	}
	values := map[string]string{
		"app.version": profile.client.version, "auth_mode": "Chatgpt", "model": profile.model,
		"originator": codexMetricOriginator(profile), "service_name": codexMetricProductService(profile),
		"session_source": codexMetricSessionSource(profile), "status": "success", "success": "true",
		"error": "none", "outcome": "success", "active": "false", "available": "true",
		"is_git": "false", "tty": "false", "cache": "miss", "compression_enabled": "false",
		"execution_mode": "sync", "handler_type": "mcp_tool", "hook_name": "Stop", "source": "plugin",
		"migration_type": "config", "tmp_mem_enabled": "false", "value": "true",
		"candidate_set_truncated": "false", "catalog_surface": "thread_context", "config_use_memories": "true",
		"db": "state", "directory": "codex_home", "event": "clear", "feature": "hooks",
		"feature_enabled": "false", "failure_reason": "write_failed", "force_refresh": "false",
		"has_citations": "false", "include_tools": "false", "kind": "response.completed",
		"method": "weighted_lexical_v1", "mode": "legacy", "observation": "installed",
		"path": "new", "phase": "open_state", "query_script": "mixed", "query_truncated": "false",
		"read_allowed": "false", "refresh": "not_requested", "reload": "false", "result": "published",
		"retained_previous_snapshot": "false", "server_kind": "openai_codex_apps", "trigger": "initial", "version": "v1",
	}
	if value := values[name]; value != "" {
		return value
	}
	return "default"
}

// codexMetricResourceService 返回客户端对应的 OTLP resource service。
func codexMetricResourceService(profile codexTelemetryProfile) string {
	if strings.EqualFold(codexClientName(profile), "Codex Desktop") {
		return "codex-app-server"
	}
	return firstNonEmptyString(profile.client.originator, "codex_cli_rs")
}

// codexMetricOriginator 按 Codex 规则清洗 originator。
func codexMetricOriginator(profile codexTelemetryProfile) string {
	value := strings.Map(func(char rune) rune {
		if char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' || strings.ContainsRune("._-/", char) {
			return char
		}
		return '_'
	}, profile.client.originator)
	value = strings.Trim(value, "_")
	if value == "" {
		return "unspecified"
	}
	if len(value) > 256 {
		return value[:256]
	}
	return value
}

// codexMetricProductService 返回 Desktop 或编辑器客户端的产品服务名。
func codexMetricProductService(profile codexTelemetryProfile) string {
	name := strings.ToLower(codexClientName(profile))
	if name == "codex desktop" {
		return "codex_desktop"
	}
	if name == "codex_vscode" {
		return "codex_vscode"
	}
	return ""
}

// codexMetricSessionSource 返回指标使用的会话来源。
func codexMetricSessionSource(profile codexTelemetryProfile) string {
	if service := codexMetricProductService(profile); service != "" {
		return "vscode"
	}
	return "cli"
}

// codexStartupMetricValue 为启动指标生成符合类型的模拟观测值。
func codexStartupMetricValue(profile codexTelemetryProfile, descriptor codexMetricDescriptor) float64 {
	if descriptor.name == "codex.turn.unified_exec.running_processes" || descriptor.name == "codex.turn.tool.call" {
		return 0
	}
	if descriptor.name == "codex.windows_mxc.available" && !strings.EqualFold(codexRuntime(profile)["runtime_os"].(string), "windows") {
		return 0
	}
	if descriptor.kind == "sum" {
		return 1
	}
	limit := 64
	if descriptor.unit == "ms" {
		limit = 4000
	} else if strings.Contains(descriptor.name, "bytes") {
		limit = 262144
	}
	return float64(simulatedInt(profile.turnID+":"+descriptor.name, limit))
}
