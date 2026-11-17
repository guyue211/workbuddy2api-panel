// wb-router：workbuddy2api 的跨域自动调度层
//
// 目的：客户端不必再写 cn:/global: 前缀。裸模型名进来后：
//  1. 结合网关 /healthz 的 realm_servable 与本地模型级惩罚表，智能决定首选域（默认 global 优先）
//  2. 首选域不可用（503/429/502/504、400 model_unavailable 或 SSE 首帧错误）→ 自动改道备选域
//  3. 保留 11140 / account_restricted_hold 风控保护，流式与非流式均不跨区重放可疑请求
//
// 带前缀的请求以该域优先，失败仍然改道（首选域若已知整域不可服务则直接改道避免白打）。
// /v1/models 会把两域模型合并成裸名列表（去重并优先保留当前活跃首选域的元数据），
// /v1/models/{id} 支持裸名跨域查找，/router/status 提供实时调度与健康观测。
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

var (
	listenAddr     = flag.String("listen", "127.0.0.1:8788", "监听地址")
	upstreamAddr   = flag.String("upstream", "127.0.0.1:8789", "真实网关地址")
	preferDomain   = flag.String("prefer", "global", "首选域（裸名先走这个）")
	altDomain      = flag.String("alt", "cn", "备选域（首选不可用时改道）")
	verbose        = flag.Bool("v", false, "打印每次调度决策")
	maxBody        = flag.Int64("max-body", 64<<20, "请求体上限（字节，超限返回 413）")
	healthInterval = flag.Duration("health-interval", 3*time.Second, "后台轮询网关 /healthz 间隔（0 关闭）")
)

var startedAt = time.Now()

var client = &http.Client{
	Transport: &http.Transport{
		MaxIdleConns:          256,
		MaxIdleConnsPerHost:   256,
		IdleConnTimeout:       90 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	},
}

// 记住「某个域对这个模型不可用」，避免每次都白试一遍。
type penaltyTable struct {
	mu sync.RWMutex
	m  map[string]time.Time
}

func newPenaltyTable() *penaltyTable {
	return &penaltyTable{m: make(map[string]time.Time)}
}

func (p *penaltyTable) mark(key string, d time.Duration) {
	now := time.Now()
	exp := now.Add(d)
	p.mu.Lock()
	if len(p.m) >= 64 {
		for k, v := range p.m {
			if !now.Before(v) {
				delete(p.m, k)
			}
		}
	}
	p.m[key] = exp
	p.mu.Unlock()
}

func (p *penaltyTable) clear(key string) {
	p.mu.Lock()
	delete(p.m, key)
	p.mu.Unlock()
}

func (p *penaltyTable) blocked(key string) bool {
	p.mu.RLock()
	exp, ok := p.m[key]
	p.mu.RUnlock()
	return ok && time.Now().Before(exp)
}

type penaltyEntry struct {
	Key    string `json:"key"`
	TTLSec int64  `json:"ttl_sec"`
}

func (p *penaltyTable) snapshot() []penaltyEntry {
	now := time.Now()
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make([]penaltyEntry, 0, len(p.m))
	for k, exp := range p.m {
		if now.Before(exp) {
			out = append(out, penaltyEntry{
				Key:    k,
				TTLSec: int64(exp.Sub(now).Seconds() + 0.999),
			})
		}
	}
	return out
}

var penalties = newPenaltyTable()

// realmHealthState 缓存网关 /healthz 暴露的 realm_servable 快照。
// 当某域因保留积分/全冷却/池空已明确 servable=false 时，直接跳过该域，
// 避免每 2 分钟向网关打一笔必败请求污染 request-logs 与面板错误率。
type realmHealthState struct {
	mu       sync.RWMutex
	servable map[string]bool
	updated  time.Time
}

const realmHealthMaxAge = 10 * time.Second

func newRealmHealthState() *realmHealthState {
	return &realmHealthState{}
}

func (rh *realmHealthState) update(servable map[string]bool) {
	cp := make(map[string]bool, len(servable))
	for k, v := range servable {
		cp[k] = v
	}
	rh.mu.Lock()
	rh.servable = cp
	rh.updated = time.Now()
	rh.mu.Unlock()
}

// markUnservable 在捕获到整域池空（no_healthy_account）时立即把该域标为短暂不可服务，
// 让其它模型也不必各自再踩一次，直到下一轮 /healthz 刷新恢复。
func (rh *realmHealthState) markUnservable(domain string) {
	rh.mu.Lock()
	if rh.servable == nil {
		rh.servable = map[string]bool{"cn": true, "global": true}
	}
	rh.servable[domain] = false
	rh.updated = time.Now()
	rh.mu.Unlock()
}

// isServable 返回 (servable, known)：缓存过期或未采样时 known=false（保守按可服务处理）。
func (rh *realmHealthState) isServable(domain string) (bool, bool) {
	rh.mu.RLock()
	defer rh.mu.RUnlock()
	if len(rh.servable) == 0 || time.Since(rh.updated) > realmHealthMaxAge {
		return true, false
	}
	v, ok := rh.servable[domain]
	if !ok {
		return true, false
	}
	return v, true
}

func (rh *realmHealthState) snapshot() (map[string]bool, int64) {
	rh.mu.RLock()
	defer rh.mu.RUnlock()
	if len(rh.servable) == 0 {
		return nil, -1
	}
	cp := make(map[string]bool, len(rh.servable))
	for k, v := range rh.servable {
		cp[k] = v
	}
	return cp, time.Since(rh.updated).Milliseconds()
}

var realmHealth = newRealmHealthState()

type domainStats struct {
	attempts atomic.Int64
	success  atomic.Int64
	errors   atomic.Int64
}

var (
	statsRequests  atomic.Int64
	statsFallbacks atomic.Int64
	statsGlobal    domainStats
	statsCN        domainStats
)

func statsFor(domain string) *domainStats {
	if domain == "cn" {
		return &statsCN
	}
	return &statsGlobal
}

func refreshRealmHealthOnce(addr string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 1200*time.Millisecond)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+"/healthz", nil)
	if err != nil {
		return false
	}
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return false
	}
	var payload struct {
		RealmServable map[string]bool `json:"realm_servable"`
	}
	if json.Unmarshal(raw, &payload) != nil || len(payload.RealmServable) == 0 {
		return false
	}
	realmHealth.update(payload.RealmServable)
	return true
}

func startHealthPoller(interval time.Duration) {
	if interval <= 0 {
		return
	}
	refreshRealmHealthOnce(*upstreamAddr)
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for range t.C {
			refreshRealmHealthOnce(*upstreamAddr)
		}
	}()
}

// effectivePreferDomain 返回当前有效首选域：若配置的 prefer 明确不可服务而 alt 可服务，则切换到 alt。
func effectivePreferDomain() string {
	pref, alt := *preferDomain, *altDomain
	if serv, known := realmHealth.isServable(pref); known && !serv {
		if altServ, altKnown := realmHealth.isServable(alt); !altKnown || altServ {
			return alt
		}
	}
	return pref
}

func hasPrefix(m string) bool {
	return strings.HasPrefix(m, "cn:") || strings.HasPrefix(m, "global:")
}

func stripPrefix(m string) string {
	m = strings.TrimPrefix(m, "cn:")
	m = strings.TrimPrefix(m, "global:")
	return m
}

// 需要跨域调度的路径（POST 且携带 JSON model 字段）
func needsRouting(r *http.Request) bool {
	if r.Method != http.MethodPost {
		return false
	}
	ct := strings.ToLower(r.Header.Get("Content-Type"))
	if strings.HasPrefix(ct, "multipart/") {
		return false
	}
	switch r.URL.Path {
	case "/v1/chat/completions", "/v1/responses", "/v1/messages", "/messages",
		"/v1/messages/count_tokens", "/messages/count_tokens",
		"/v1/images/generations", "/v1/videos", "/v1/videos/generations":
		return true
	}
	return false
}

func copyRequestHeaders(dst *http.Request, src *http.Request) {
	for k, vv := range src.Header {
		if strings.EqualFold(k, "Host") || strings.EqualFold(k, "Content-Length") {
			continue
		}
		for _, v := range vv {
			dst.Header.Add(k, v)
		}
	}
}

func copyResponseHeaders(w http.ResponseWriter, resp *http.Response) {
	for k, vv := range resp.Header {
		if strings.EqualFold(k, "Content-Length") || strings.EqualFold(k, "Transfer-Encoding") {
			continue
		}
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}
}

// 逐块抄响应体并立即 flush —— SSE 流式必须这样，否则客户端看不到增量。
func relay(w http.ResponseWriter, resp *http.Response) {
	defer resp.Body.Close()
	copyResponseHeaders(w, resp)
	w.WriteHeader(resp.StatusCode)

	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 32*1024)
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if err != nil {
			return
		}
	}
}

// 纯透传：/panel/、/status、/healthz 等
func proxyPlain(w http.ResponseWriter, r *http.Request) {
	target := "http://" + *upstreamAddr + r.URL.RequestURI()
	req, err := http.NewRequestWithContext(r.Context(), r.Method, target, r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	copyRequestHeaders(req, r)

	resp, err := client.Do(req)
	if err != nil {
		http.Error(w, "upstream unreachable: "+err.Error(), http.StatusBadGateway)
		return
	}
	relay(w, resp)
}

func isJSONSpace(b byte) bool {
	return b == ' ' || b == '\t' || b == '\r' || b == '\n'
}

// scanJSONString 扫描从 quoteIdx（必须是 '"'）开始的 JSON 字符串，返回闭合引号后的下一个下标。
func scanJSONString(b []byte, quoteIdx int) (int, bool) {
	if quoteIdx >= len(b) || b[quoteIdx] != '"' {
		return 0, false
	}
	for i := quoteIdx + 1; i < len(b); i++ {
		if b[i] == '\\' {
			i++
			continue
		}
		if b[i] == '"' {
			return i + 1, true
		}
	}
	return 0, false
}

// skipJSONValue 跳过从 idx 开始的一个完整 JSON 值，返回该值结束后的下标。
func skipJSONValue(b []byte, idx int) (int, bool) {
	for idx < len(b) && isJSONSpace(b[idx]) {
		idx++
	}
	if idx >= len(b) {
		return 0, false
	}
	switch b[idx] {
	case '"':
		return scanJSONString(b, idx)
	case '{', '[':
		depth := 1
		for i := idx + 1; i < len(b); i++ {
			switch b[i] {
			case '"':
				next, ok := scanJSONString(b, i)
				if !ok {
					return 0, false
				}
				i = next - 1
			case '{', '[':
				depth++
			case '}', ']':
				depth--
				if depth == 0 {
					return i + 1, true
				}
			}
		}
		return 0, false
	default:
		for i := idx; i < len(b); i++ {
			c := b[i]
			if isJSONSpace(c) || c == ',' || c == '}' || c == ']' {
				return i, true
			}
		}
		return len(b), true
	}
}

// findTopLevelModel 零分配扫描顶层 JSON 对象的 "model" 字符串字段位置 [valStart, valEnd)。
// 避免对几 MB 的长对话请求做整包 Unmarshal + Marshal（既省 75%+ 内存峰值，又严格保持顶层键顺序）。
func findTopLevelModel(body []byte) (model string, valStart, valEnd int, ok bool) {
	i := 0
	for i < len(body) && isJSONSpace(body[i]) {
		i++
	}
	if i >= len(body) || body[i] != '{' {
		return "", 0, 0, false
	}
	i++
	for i < len(body) {
		for i < len(body) && (isJSONSpace(body[i]) || body[i] == ',') {
			i++
		}
		if i >= len(body) || body[i] == '}' {
			return "", 0, 0, false
		}
		if body[i] != '"' {
			return "", 0, 0, false
		}
		keyEnd, sok := scanJSONString(body, i)
		if !sok {
			return "", 0, 0, false
		}
		rawKey := body[i:keyEnd]
		i = keyEnd
		for i < len(body) && isJSONSpace(body[i]) {
			i++
		}
		if i >= len(body) || body[i] != ':' {
			return "", 0, 0, false
		}
		i++
		for i < len(body) && isJSONSpace(body[i]) {
			i++
		}
		if i >= len(body) {
			return "", 0, 0, false
		}
		isModelKey := string(rawKey) == `"model"`
		if !isModelKey && bytes.Contains(rawKey, []byte(`\u`)) {
			var k string
			if json.Unmarshal(rawKey, &k) == nil && k == "model" {
				isModelKey = true
			}
		}
		if isModelKey {
			if body[i] != '"' {
				return "", 0, 0, false
			}
			vEnd, vok := scanJSONString(body, i)
			if !vok {
				return "", 0, 0, false
			}
			var val string
			if json.Unmarshal(body[i:vEnd], &val) != nil {
				return "", 0, 0, false
			}
			return val, i, vEnd, true
		}
		next, vok := skipJSONValue(body, i)
		if !vok {
			return "", 0, 0, false
		}
		i = next
	}
	return "", 0, 0, false
}

func spliceModel(body []byte, valStart, valEnd int, fullModel string) ([]byte, error) {
	nb, err := json.Marshal(fullModel)
	if err != nil {
		return nil, err
	}
	out := make([]byte, 0, valStart+len(nb)+(len(body)-valEnd))
	out = append(out, body[:valStart]...)
	out = append(out, nb...)
	out = append(out, body[valEnd:]...)
	return out, nil
}

// 把 body 里的 model 改写成 "<domain>:<bare>"（兜底慢路径）
func rewriteModel(body []byte, fullModel string) ([]byte, error) {
	if _, vs, ve, ok := findTopLevelModel(body); ok {
		return spliceModel(body, vs, ve, fullModel)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, err
	}
	nb, err := json.Marshal(fullModel)
	if err != nil {
		return nil, err
	}
	m["model"] = nb
	return json.Marshal(m)
}

func send(r *http.Request, body []byte) (*http.Response, error) {
	target := "http://" + *upstreamAddr + r.URL.RequestURI()
	req, err := http.NewRequestWithContext(r.Context(), r.Method, target, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	copyRequestHeaders(req, r)
	req.ContentLength = int64(len(body))
	req.Header.Del("Content-Length")
	return client.Do(req)
}

func isRestrictionBody(body []byte) bool {
	return bytes.Contains(body, []byte("account_restricted_hold")) || bytes.Contains(body, []byte("11140"))
}

// 这个失败能不能靠换域救回来
func shouldFallback(status int, body []byte) bool {
	// 与流式侧保持同一风控保护：11140 / account_restricted_hold 可能由问题请求体触发，不跨区重试以免连坐另一域。
	if isRestrictionBody(body) {
		return false
	}
	switch status {
	case http.StatusServiceUnavailable, // 503 池空/全冷却
		http.StatusTooManyRequests, // 429
		http.StatusBadGateway,      // 502
		http.StatusGatewayTimeout:  // 504
		return true
	}
	if status == http.StatusBadRequest && bytes.Contains(body, []byte("model_unavailable")) {
		return true
	}
	return false
}

func computeDomainOrder(model, bare string) []string {
	var first, other string
	if hasPrefix(model) {
		first = *preferDomain
		if strings.HasPrefix(model, "cn:") {
			first = "cn"
		} else if strings.HasPrefix(model, "global:") {
			first = "global"
		}
		other = *altDomain
		if first == *altDomain {
			other = *preferDomain
		}
		if other == first {
			other = "cn"
			if first == "cn" {
				other = "global"
			}
		}
	} else {
		first = *preferDomain
		other = *altDomain
	}
	// 1. 域级健康感知：若首选域明确整域不可服务（如全员低于保留积分或全冷却）且备选域可服务，直接先走备选域。
	if serv, known := realmHealth.isServable(first); known && !serv {
		if altServ, altKnown := realmHealth.isServable(other); !altKnown || altServ {
			return []string{other, first}
		}
	}
	// 2. 模型级惩罚记忆：首选域刚在该模型上失败过（且备选域未被证明同样受罚或不可服务），先走备选域。
	if penalties.blocked(first + ":" + bare) {
		if serv, known := realmHealth.isServable(other); !known || serv {
			return []string{other, first}
		}
	}
	return []string{first, other}
}

func handleChat(w http.ResponseWriter, r *http.Request) {
	limit := *maxBody
	if limit <= 0 {
		limit = 64 << 20
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	if err != nil {
		http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
		return
	}
	if int64(len(body)) > limit {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusRequestEntityTooLarge)
		_, _ = w.Write([]byte(`{"error":{"message":"request body exceeds router max-body limit","type":"invalid_request_error","code":"payload_too_large"}}`))
		return
	}

	model, valStart, valEnd, fastOK := findTopLevelModel(body)
	if !fastOK {
		var probe struct {
			Model string `json:"model"`
		}
		if err := json.Unmarshal(body, &probe); err != nil {
			// 解析不了就原样转发，让网关自己报错
			resp, serr := send(r, body)
			if serr != nil {
				http.Error(w, serr.Error(), http.StatusBadGateway)
				return
			}
			relay(w, resp)
			return
		}
		model = probe.Model
	}

	// 没有 model 字段：不干预
	if model == "" {
		resp, serr := send(r, body)
		if serr != nil {
			http.Error(w, serr.Error(), http.StatusBadGateway)
			return
		}
		relay(w, resp)
		return
	}

	statsRequests.Add(1)
	bare := stripPrefix(model)
	order := computeDomainOrder(model, bare)

	for i, domain := range order {
		full := domain + ":" + bare
		var nb []byte
		var rerr error
		if fastOK {
			nb, rerr = spliceModel(body, valStart, valEnd, full)
		} else {
			nb, rerr = rewriteModel(body, full)
		}
		if rerr != nil {
			http.Error(w, "rewrite model: "+rerr.Error(), http.StatusInternalServerError)
			return
		}

		ds := statsFor(domain)
		ds.attempts.Add(1)
		resp, serr := send(r, nb)
		if serr != nil {
			ds.errors.Add(1)
			if i == len(order)-1 {
				http.Error(w, "upstream unreachable: "+serr.Error(), http.StatusBadGateway)
				return
			}
			statsFallbacks.Add(1)
			if *verbose {
				log.Printf("[router] %s %s -> dial error, trying next", bare, domain)
			}
			continue
		}

		// HTTP 200 也可能是 SSE 错误；提交响应前先检查首个有效事件。
		if resp.StatusCode == http.StatusOK {
			retry, streamErr, poolEmpty := inspectStreamDetailed(resp)
			if r.Context().Err() != nil {
				resp.Body.Close()
				return
			}
			if (retry || streamErr != nil) && i < len(order)-1 {
				ds.errors.Add(1)
				statsFallbacks.Add(1)
				resp.Body.Close()
				penalties.mark(full, 2*time.Minute)
				if poolEmpty {
					realmHealth.markUnservable(domain)
				}
				if *verbose {
					log.Printf("[router] %s -> %s stream failed before output, falling back to %s", bare, domain, order[i+1])
				}
				continue
			}
			ds.success.Add(1)
			penalties.clear(full)
			w.Header().Set("X-Wb-Router-Domain", domain)
			if i > 0 {
				w.Header().Set("X-Wb-Router-Fallback", "1")
			}
			if *verbose {
				log.Printf("[router] %s -> %s (200, acct=%s)", bare, domain, resp.Header.Get("X-Wb-Account"))
			}
			relay(w, resp)
			return
		}

		// 失败：先读完错误体，判断能不能换域
		ds.errors.Add(1)
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		if bytes.Contains(errBody, []byte("no_healthy_account")) && !isRestrictionBody(errBody) {
			realmHealth.markUnservable(domain)
		}

		last := i == len(order)-1
		if !last && shouldFallback(resp.StatusCode, errBody) {
			statsFallbacks.Add(1)
			penalties.mark(full, 2*time.Minute)
			if *verbose {
				log.Printf("[router] %s -> %s failed %d, falling back to %s", bare, domain, resp.StatusCode, order[i+1])
			}
			continue
		}

		// 不能救，或已经试完：原样把错误给客户端
		if *verbose {
			log.Printf("[router] %s -> %s failed %d (no fallback)", bare, domain, resp.StatusCode)
		}
		w.Header().Set("X-Wb-Router-Domain", domain)
		copyResponseHeaders(w, resp)
		w.Header().Set("Content-Length", itoa(len(errBody)))
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(errBody)
		return
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

// deduplicateBareModels 将两域模型合并为裸名列表；同名模型同时存在时，优先保留当前活跃首选域（effectivePreferDomain）的元数据。
func deduplicateBareModels(items []map[string]json.RawMessage, activePrefer string) []map[string]json.RawMessage {
	preferPrefix := activePrefer + ":"
	idxByBare := make(map[string]int, len(items))
	fromPreferred := make(map[string]bool, len(items))
	out := make([]map[string]json.RawMessage, 0, len(items))

	for _, it := range items {
		var id string
		if err := json.Unmarshal(it["id"], &id); err != nil {
			continue
		}
		bare := stripPrefix(id)
		if bare == "" {
			continue
		}
		isPref := strings.HasPrefix(id, preferPrefix)
		nid, _ := json.Marshal(bare)
		cloned := make(map[string]json.RawMessage, len(it))
		for k, v := range it {
			cloned[k] = v
		}
		cloned["id"] = nid

		if pos, seen := idxByBare[bare]; seen {
			if isPref && !fromPreferred[bare] {
				out[pos] = cloned
				fromPreferred[bare] = true
			}
			continue
		}
		idxByBare[bare] = len(out)
		fromPreferred[bare] = isPref
		out = append(out, cloned)
	}
	return out
}

// /v1/models：把 cn:/global: 两域合并成裸名列表（同名优先取当前有效首选域的条目），
// 客户端看到的全是裸名，调用时由本层自动选域。
func proxyModels(w http.ResponseWriter, r *http.Request) {
	target := "http://" + *upstreamAddr + r.URL.RequestURI()
	req, err := http.NewRequestWithContext(r.Context(), r.Method, target, nil)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	copyRequestHeaders(req, r)

	resp, err := client.Do(req)
	if err != nil {
		http.Error(w, "upstream unreachable: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if resp.StatusCode != http.StatusOK {
		copyResponseHeaders(w, resp)
		w.Header().Set("Content-Length", itoa(len(raw)))
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(raw)
		return
	}

	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		copyResponseHeaders(w, resp)
		w.Header().Set("Content-Length", itoa(len(raw)))
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(raw)
		return
	}

	var items []map[string]json.RawMessage
	if err := json.Unmarshal(top["data"], &items); err != nil {
		copyResponseHeaders(w, resp)
		w.Header().Set("Content-Length", itoa(len(raw)))
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(raw)
		return
	}

	out := deduplicateBareModels(items, effectivePreferDomain())
	newData, _ := json.Marshal(out)
	top["data"] = newData
	final, _ := json.Marshal(top)

	copyResponseHeaders(w, resp)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", itoa(len(final)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(final)
}

// GET /v1/models/{model}：OpenAI SDK 常调单模型查询，从合并目录里按裸名或带前缀名查找并返回。
func proxySingleModel(w http.ResponseWriter, r *http.Request) {
	wantID := strings.TrimPrefix(r.URL.Path, "/v1/models/")
	if wantID == "" || strings.Contains(wantID, "/") {
		proxyPlain(w, r)
		return
	}
	target := "http://" + *upstreamAddr + "/v1/models"
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, target, nil)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	copyRequestHeaders(req, r)

	resp, err := client.Do(req)
	if err != nil {
		http.Error(w, "upstream unreachable: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if resp.StatusCode != http.StatusOK {
		copyResponseHeaders(w, resp)
		w.Header().Set("Content-Length", itoa(len(raw)))
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(raw)
		return
	}

	var top struct {
		Data []map[string]json.RawMessage `json:"data"`
	}
	if json.Unmarshal(raw, &top) != nil {
		proxyPlain(w, r)
		return
	}

	if hasPrefix(wantID) {
		for _, it := range top.Data {
			var id string
			if json.Unmarshal(it["id"], &id) == nil && id == wantID {
				final, _ := json.Marshal(it)
				copyResponseHeaders(w, resp)
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Content-Length", itoa(len(final)))
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write(final)
				return
			}
		}
	} else {
		merged := deduplicateBareModels(top.Data, effectivePreferDomain())
		for _, it := range merged {
			var id string
			if json.Unmarshal(it["id"], &id) == nil && id == wantID {
				final, _ := json.Marshal(it)
				copyResponseHeaders(w, resp)
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Content-Length", itoa(len(final)))
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write(final)
				return
			}
		}
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusNotFound)
	_, _ = w.Write([]byte(`{"error":{"message":"model not found","type":"invalid_request_error","code":"model_not_found"}}`))
}

func isLoopbackRemote(remoteAddr string) bool {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// verifyRouterStatusAuth 非回环访问 /router/status 时复用网关 /status 的鉴权校验，防止未授权泄露状态。
func verifyRouterStatusAuth(r *http.Request) bool {
	if isLoopbackRemote(r.RemoteAddr) && r.Header.Get("X-Forwarded-For") == "" && r.Header.Get("X-Real-IP") == "" {
		return true
	}
	target := "http://" + *upstreamAddr + "/status"
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, target, nil)
	if err != nil {
		return false
	}
	copyRequestHeaders(req, r)
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// GET /router/status：跨域路由器实时运行态、域级可用性缓存、分域吞吐与活跃模型惩罚表。
func handleRouterStatus(w http.ResponseWriter, r *http.Request) {
	if !verifyRouterStatusAuth(r) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"unauthorized","type":"authentication_error"}}`))
		return
	}
	serv, ageMs := realmHealth.snapshot()
	out := map[string]any{
		"service":            "wb-router",
		"uptime_sec":         int64(time.Since(startedAt).Seconds()),
		"listen":             *listenAddr,
		"upstream":           *upstreamAddr,
		"prefer_domain":      *preferDomain,
		"alt_domain":         *altDomain,
		"effective_prefer":   effectivePreferDomain(),
		"realm_servable":     serv,
		"health_cache_age_ms": ageMs,
		"requests_total":     statsRequests.Load(),
		"fallbacks_total":    statsFallbacks.Load(),
		"by_domain": map[string]any{
			"global": map[string]int64{
				"attempts": statsGlobal.attempts.Load(),
				"success":  statsGlobal.success.Load(),
				"errors":   statsGlobal.errors.Load(),
			},
			"cn": map[string]int64{
				"attempts": statsCN.attempts.Load(),
				"success":  statsCN.success.Load(),
				"errors":   statsCN.errors.Load(),
			},
		},
		"active_penalties": penalties.snapshot(),
	}
	raw, _ := json.Marshal(out)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", itoa(len(raw)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(raw)
}

func handler(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/router/status" && r.Method == http.MethodGet {
		handleRouterStatus(w, r)
		return
	}
	if r.URL.Path == "/v1/models" && r.Method == http.MethodGet {
		proxyModels(w, r)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/v1/models/") && r.Method == http.MethodGet {
		proxySingleModel(w, r)
		return
	}
	if needsRouting(r) {
		handleChat(w, r)
		return
	}
	proxyPlain(w, r)
}

func main() {
	flag.Parse()
	log.SetFlags(log.LstdFlags)
	startHealthPoller(*healthInterval)
	log.Printf("wb-router listening on %s -> %s (裸名先走 %s，失败改道 %s，健康轮询 %s)",
		*listenAddr, *upstreamAddr, *preferDomain, *altDomain, *healthInterval)

	srv := &http.Server{
		Addr:              *listenAddr,
		Handler:           http.HandlerFunc(handler),
		ReadHeaderTimeout: 30 * time.Second,
		// 不设 WriteTimeout：SSE 长连接会被切断
	}
	if err := srv.ListenAndServe(); err != nil {
		log.Fatal(err)
	}
}
