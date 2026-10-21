package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// External voice-port probe. The panel runs on the same host as the TS3
// containers, so it can prove one direction locally: if nothing is bound to the
// UDP port, the port cannot be reachable from outside either. Proving that a
// listening UDP port is actually reachable through a cloud security group
// requires an outside observer, so the default probe asks check-host.net from an
// external node. Every inconclusive outcome is reported as "unknown" — a
// missing probe result must never be rendered as "closed".
const (
	portCheckOpen    = "open"
	portCheckClosed  = "closed"
	portCheckUnknown = "unknown"

	defaultCheckHostBaseURL = "https://check-host.net"
	defaultCheckPollEvery   = time.Second
	defaultCheckWait        = 8 * time.Second
	defaultCheckHTTPTimeout = 6 * time.Second
	defaultCheckWorkers     = 8
	defaultCheckRequestTTL  = 12 * time.Second
)

type portProbeResult struct {
	State  string
	Detail string
}

// voicePortProbe is the seam used by tests. Production builds use
// checkHostVoiceProbe; tests inject deterministic results and never touch the
// network.
type voicePortProbe interface {
	Check(ctx context.Context, publicIP string, port int) portProbeResult
}

type portCheckItem struct {
	Port         int    `json:"port"`
	InstanceID   string `json:"instance_id"`
	InstanceName string `json:"instance_name"`
	Protocol     string `json:"protocol"`
	State        string `json:"state"`
	Detail       string `json:"detail"`
}

type portsCheckResponse struct {
	PublicIP     string          `json:"public_ip,omitempty"`
	Enabled      bool            `json:"enabled"`
	CacheSeconds int             `json:"cache_seconds"`
	CheckedAt    time.Time       `json:"checked_at"`
	Results      []portCheckItem `json:"results"`
	Note         string          `json:"note"`
}

const portCheckNote = "本功能由第三方 check-host.net 探测；服务不可用或结果不明确时显示“未验证”，不会误报为不通。检测结果为辅助参考，最终以客户端能否连接为准。"

// voiceProbeCache keeps probe answers for a short while so repeated page loads
// do not hammer check-host.net. The cache is keyed by public IP + port.
type voiceProbeCache struct {
	mu      sync.Mutex
	entries map[string]probeCacheEntry
}

type probeCacheEntry struct {
	result portProbeResult
	at     time.Time
}

func newVoiceProbeCache() *voiceProbeCache {
	return &voiceProbeCache{entries: make(map[string]probeCacheEntry)}
}

func (c *voiceProbeCache) get(key string, ttl time.Duration, now time.Time) (portProbeResult, bool) {
	if c == nil || ttl <= 0 {
		return portProbeResult{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[key]
	if !ok || now.Sub(entry.at) >= ttl {
		return portProbeResult{}, false
	}
	return entry.result, true
}

func (c *voiceProbeCache) put(key string, result portProbeResult, now time.Time) {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.entries[key] = probeCacheEntry{result: result, at: now}
	c.mu.Unlock()
}

func (c *voiceProbeCache) clear() {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.entries = make(map[string]probeCacheEntry)
	c.mu.Unlock()
}

// handlePortsCheck probes every instance's public voice port concurrently. It
// reads state.json directly: the check does not need Docker and must still work
// when the daemon is briefly unavailable.
func (a *API) handlePortsCheck(w http.ResponseWriter, r *http.Request) {
	st, err := a.state.Snapshot()
	if err != nil {
		a.internalError(w, "port connectivity check", err)
		return
	}

	cfg := a.config.Snapshot()
	publicIP := strings.TrimSpace(cfg.Cloudflare.PublicIP)
	enabled := cfg.Ports.ExternalCheck
	cacheSeconds := cfg.Ports.CheckCacheSeconds
	if cacheSeconds <= 0 {
		cacheSeconds = 60
	}

	targets := make([]portCheckItem, 0, len(st.Instances))
	for _, inst := range st.Instances {
		if inst.Ports.Voice <= 0 {
			continue
		}
		targets = append(targets, portCheckItem{
			Port:         inst.Ports.Voice,
			InstanceID:   inst.ID,
			InstanceName: inst.Name,
			Protocol:     portProtocolUDP,
		})
	}
	sort.Slice(targets, func(i, j int) bool { return targets[i].Port < targets[j].Port })

	note := portCheckNote
	if len(targets) == 0 {
		writeJSON(w, http.StatusOK, portsCheckResponse{
			PublicIP:     publicIP,
			Enabled:      enabled,
			CacheSeconds: cacheSeconds,
			CheckedAt:    time.Now().UTC(),
			Results:      []portCheckItem{},
			Note:         "当前没有实例，无需检测。",
		})
		return
	}
	if !enabled {
		results := make([]portCheckItem, len(targets))
		for i, item := range targets {
			item.State = portCheckUnknown
			item.Detail = "第三方端口探测已关闭（可在设置页开启 check-host.net 探测）。"
			results[i] = item
		}
		writeJSON(w, http.StatusOK, portsCheckResponse{
			PublicIP:     publicIP,
			Enabled:      false,
			CacheSeconds: cacheSeconds,
			CheckedAt:    time.Now().UTC(),
			Results:      results,
			Note:         "第三方端口探测当前为关闭状态，因此端口状态显示为“未验证”，这不代表端口不通。可在设置页开启后重新检测。",
		})
		return
	}
	if publicIP == "" {
		note = "未配置公网 IP，无法从外部探测；结果中的“未验证”不代表端口不通。请先在设置页填写公网 IP。" + note
	}
	note = fmt.Sprintf("%s（结果缓存 %d 秒）", note, cacheSeconds)

	ctx, cancel := context.WithTimeout(r.Context(), defaultCheckRequestTTL)
	defer cancel()

	probe := a.voiceProbe
	results := make([]portCheckItem, len(targets))
	ttl := time.Duration(cacheSeconds) * time.Second
	sem := make(chan struct{}, defaultCheckWorkers)
	var wg sync.WaitGroup
	for i := range targets {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			item := targets[index]
			if probe == nil {
				item.State = portCheckUnknown
				item.Detail = "当前进程没有配置端口探测能力。"
				results[index] = item
				return
			}
			probeResult := a.probeWithCache(ctx, publicIP, item.Port, ttl)
			item.State = normalizePortState(probeResult.State)
			item.Detail = strings.TrimSpace(probeResult.Detail)
			results[index] = item
		}(i)
	}
	wg.Wait()

	writeJSON(w, http.StatusOK, portsCheckResponse{
		PublicIP:     publicIP,
		Enabled:      true,
		CacheSeconds: cacheSeconds,
		CheckedAt:    time.Now().UTC(),
		Results:      results,
		Note:         note,
	})
}

// probeWithCache serves a cached answer when one is fresh and otherwise runs
// the injected probe, storing its result for the configured TTL.
func (a *API) probeWithCache(ctx context.Context, publicIP string, port int, ttl time.Duration) portProbeResult {
	key := fmt.Sprintf("%s|%d", publicIP, port)
	now := time.Now()
	if cached, ok := a.probeCache.get(key, ttl, now); ok {
		return cached
	}
	result := a.voiceProbe.Check(ctx, publicIP, port)
	a.probeCache.put(key, result, now)
	return result
}

func normalizePortState(state string) string {
	switch state {
	case portCheckOpen, portCheckClosed, portCheckUnknown:
		return state
	default:
		return portCheckUnknown
	}
}

// checkHostVoiceProbe implements voicePortProbe with a local bind test plus the
// public check-host.net UDP check API.
type checkHostVoiceProbe struct {
	client    *http.Client
	baseURL   string
	pollEvery time.Duration
	wait      time.Duration
}

func newVoicePortProbe() voicePortProbe {
	return &checkHostVoiceProbe{
		client:    &http.Client{Timeout: defaultCheckHTTPTimeout},
		baseURL:   defaultCheckHostBaseURL,
		pollEvery: defaultCheckPollEvery,
		wait:      defaultCheckWait,
	}
}

func (p *checkHostVoiceProbe) Check(ctx context.Context, publicIP string, port int) portProbeResult {
	if port <= 0 || port > 65535 {
		return portProbeResult{State: portCheckUnknown, Detail: "端口号无效，无法检测。"}
	}
	if !udpPortInUse(port) {
		return portProbeResult{
			State:  portCheckClosed,
			Detail: fmt.Sprintf("本机 UDP %d 没有进程监听（实例可能已停止）。", port),
		}
	}
	if strings.TrimSpace(publicIP) == "" {
		return portProbeResult{
			State:  portCheckUnknown,
			Detail: "本机端口正在监听，但未配置公网 IP，无法从外部确认安全组是否放行。",
		}
	}
	return p.checkExternal(ctx, strings.TrimSpace(publicIP), port)
}

// udpPortInUse reports whether binding 0.0.0.0:port fails. A successful bind
// proves that no TS3 container is listening; the listener is closed immediately
// so the probe does not disturb the running server. An error caused by
// permissions or an unsupported host is treated as "possibly in use" so the
// caller falls through to the external probe rather than claiming "closed".
func udpPortInUse(port int) bool {
	conn, err := net.ListenPacket("udp4", "0.0.0.0:"+strconv.Itoa(port))
	if err != nil {
		return true
	}
	_ = conn.Close()
	return false
}

// checkExternal asks check-host.net for an outside-in UDP probe.
//
// TODO(M5): confirm the UDP result schema against a real response. The public
// API documentation lists "udp" as a check type but shows result examples only
// for ping/http/tcp/dns, so unrecognised shapes deliberately fall back to
// unknown instead of guessing.
func (p *checkHostVoiceProbe) checkExternal(ctx context.Context, publicIP string, port int) portProbeResult {
	host := net.JoinHostPort(publicIP, strconv.Itoa(port))
	endpoint := p.baseURL + "/check-udp?host=" + url.QueryEscape(host) + "&max_nodes=1"

	var start struct {
		OK        int    `json:"ok"`
		RequestID string `json:"request_id"`
		Error     string `json:"error"`
	}
	if err := p.getJSON(ctx, endpoint, &start); err != nil {
		return portProbeResult{State: portCheckUnknown, Detail: "无法连接公共探测服务，端口状态未验证。"}
	}
	if start.Error != "" {
		if strings.Contains(strings.ToLower(start.Error), "limit") {
			return portProbeResult{State: portCheckUnknown, Detail: "公共探测服务限流，请稍后再试。"}
		}
		return portProbeResult{State: portCheckUnknown, Detail: "公共探测服务暂时不可用，端口状态未验证。"}
	}
	if start.RequestID == "" {
		return portProbeResult{State: portCheckUnknown, Detail: "公共探测服务没有返回任务编号，端口状态未验证。"}
	}

	resultURL := p.baseURL + "/check-result/" + url.PathEscape(start.RequestID)
	deadline := time.Now().Add(p.wait)
	for {
		if !sleepContext(ctx, p.pollEvery) {
			return portProbeResult{State: portCheckUnknown, Detail: "检测超时，端口状态未验证。"}
		}
		var raw map[string]any
		if err := p.getJSON(ctx, resultURL, &raw); err != nil {
			return portProbeResult{State: portCheckUnknown, Detail: "无法读取公共探测结果，端口状态未验证。"}
		}
		state, detail, done := interpretCheckHostResults(raw)
		if done {
			return portProbeResult{State: state, Detail: detail}
		}
		if time.Now().After(deadline) {
			return portProbeResult{State: portCheckUnknown, Detail: "公共探测服务未在时限内返回结果，端口状态未验证。"}
		}
	}
}

func (p *checkHostVoiceProbe) getJSON(ctx context.Context, endpoint string, dst any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "ts3panel-portcheck/1.0")
	resp, err := p.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("unexpected status %d", resp.StatusCode)
	}
	dec := json.NewDecoder(io.LimitReader(resp.Body, 1<<20))
	return dec.Decode(dst)
}

func sleepContext(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// interpretCheckHostResults turns the loosely typed check-result document into
// one of three states. Only an explicit response (a time value) counts as open;
// only an explicit refusal/unreachable error counts as closed. Pending nodes
// keep the caller polling, and timeouts stay unknown because UDP silence is
// indistinguishable from a firewall drop.
func interpretCheckHostResults(raw map[string]any) (state, detail string, done bool) {
	if len(raw) == 0 {
		return "", "", false
	}

	pending := false
	for _, node := range raw {
		nodeState, nodeDetail, nodePending := classifyCheckHostNode(node)
		if nodePending {
			pending = true
			continue
		}
		if nodeState == portCheckOpen {
			return portCheckOpen, nodeDetail, true
		}
		if nodeState == portCheckClosed {
			return portCheckClosed, nodeDetail, true
		}
		// Unknown is a resolved-but-inconclusive answer. Keep looking at other
		// nodes so one broken node cannot mask a positive result.
		if !pending && detail == "" {
			state, detail = portCheckUnknown, nodeDetail
		}
	}
	if pending {
		return "", "", false
	}
	if state == "" {
		return portCheckUnknown, "公共探测服务的结果无法判定端口状态。", true
	}
	return state, detail, true
}

func classifyCheckHostNode(value any) (state, detail string, pending bool) {
	switch node := value.(type) {
	case nil:
		return "", "", true
	case []any:
		if len(node) == 0 {
			return "", "", true
		}
		sawPending := false
		for _, item := range node {
			itemState, itemDetail, itemPending := classifyCheckHostNode(item)
			if itemPending {
				sawPending = true
				continue
			}
			if itemState == portCheckOpen || itemState == portCheckClosed {
				return itemState, itemDetail, false
			}
			if detail == "" {
				state, detail = itemState, itemDetail
			}
		}
		if sawPending {
			return "", "", true
		}
		if state == "" {
			state = portCheckUnknown
			detail = "公共探测服务的结果无法判定端口状态。"
		}
		return state, detail, false
	case map[string]any:
		if value, ok := node["time"]; ok && value != nil {
			return portCheckOpen, "探测节点收到了 UDP 响应。", false
		}
		if errValue, ok := node["error"]; ok {
			return classifyProbeError(fmt.Sprint(errValue))
		}
		return portCheckUnknown, "公共探测服务的结果无法判定端口状态。", false
	case string:
		switch strings.ToUpper(strings.TrimSpace(node)) {
		case "OK", "SUCCESS", "OPEN":
			return portCheckOpen, "探测节点收到了 UDP 响应。", false
		case "TIMEOUT", "MALFORMED", "FAILED":
			return portCheckUnknown, "探测节点未取得可判定的响应，端口状态未验证。", false
		default:
			return portCheckUnknown, "公共探测服务的结果无法判定端口状态。", false
		}
	case float64:
		if node > 0 {
			return portCheckOpen, "探测节点收到了 UDP 响应。", false
		}
		return portCheckUnknown, "公共探测服务的结果无法判定端口状态。", false
	default:
		return portCheckUnknown, "公共探测服务的结果无法判定端口状态。", false
	}
}

func classifyProbeError(message string) (state, detail string, pending bool) {
	lower := strings.ToLower(message)
	switch {
	// ICMP port unreachable is the only UDP signal that reliably means
	// "nothing is listening". Host/network unreachable and plain timeouts can
	// also be caused by routing or firewall drops, so they stay unknown.
	case strings.Contains(lower, "refused"),
		strings.Contains(lower, "port unreachable"):
		return portCheckClosed, "探测节点报告端口不可达。", false
	case strings.Contains(lower, "timeout"), strings.Contains(lower, "timed out"):
		return portCheckUnknown, "探测节点等待响应超时，无法区分未开放与防火墙丢弃。", false
	default:
		return portCheckUnknown, "探测节点返回了无法判定的错误，端口状态未验证。", false
	}
}
