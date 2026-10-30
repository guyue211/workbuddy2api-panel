// wb-router：workbuddy2api 的跨域自动调度层
//
// 目的：客户端不必再写 cn:/global: 前缀。裸模型名进来后：
//  1. 默认先走 global（国际版）
//  2. global 池不可用（503/429/502/504，或 400 model_unavailable）→ 自动改道 cn
//  3. cn 也不可用 → 再试回 global（最后一次）
//
// 带前缀的请求以该域优先，失败仍然改道。/v1/models 会把两域模型合并成裸名列表（去重），
// 这样客户端列表里看到的就是裸名，配合自动调度形成闭环。
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

var (
	listenAddr   = flag.String("listen", "127.0.0.1:8788", "监听地址")
	upstreamAddr = flag.String("upstream", "127.0.0.1:8789", "真实网关地址")
	preferDomain = flag.String("prefer", "global", "首选域（裸名先走这个）")
	altDomain    = flag.String("alt", "cn", "备选域（首选不可用时改道）")
	verbose      = flag.Bool("v", false, "打印每次调度决策")
	maxBody      = flag.Int64("max-body", 16<<20, "请求体上限（字节）")
)

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
	p.mu.Lock()
	p.m[key] = time.Now().Add(d)
	p.mu.Unlock()
}

func (p *penaltyTable) blocked(key string) bool {
	p.mu.RLock()
	exp, ok := p.m[key]
	p.mu.RUnlock()
	return ok && time.Now().Before(exp)
}

var penalties = newPenaltyTable()

func hasPrefix(m string) bool {
	return strings.HasPrefix(m, "cn:") || strings.HasPrefix(m, "global:")
}

func stripPrefix(m string) string {
	m = strings.TrimPrefix(m, "cn:")
	m = strings.TrimPrefix(m, "global:")
	return m
}

// 需要跨域调度的路径（都是 POST 且 body 里带 model）
func needsRouting(r *http.Request) bool {
	if r.Method != http.MethodPost {
		return false
	}
	switch r.URL.Path {
	case "/v1/chat/completions", "/v1/responses", "/v1/messages", "/messages":
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

// 把 body 里的 model 改写成 "<domain>:<bare>"
func rewriteModel(body []byte, fullModel string) ([]byte, error) {
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

func send(w http.ResponseWriter, r *http.Request, body []byte) (*http.Response, error) {
	target := "http://" + *upstreamAddr + r.URL.RequestURI()
	req, err := http.NewRequestWithContext(r.Context(), r.Method, target, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	copyRequestHeaders(req, r)
	req.ContentLength = int64(len(body))
	req.Header.Set("Content-Length", "")
	return client.Do(req)
}

// 这个失败能不能靠换域救回来
func shouldFallback(status int, body []byte) bool {
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

func handleChat(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, *maxBody))
	if err != nil {
		http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
		return
	}

	var probe struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		// 解析不了就原样转发，让网关自己报错
		resp, serr := send(w, r, body)
		if serr != nil {
			http.Error(w, serr.Error(), http.StatusBadGateway)
			return
		}
		relay(w, resp)
		return
	}

	model := probe.Model

	// 没有 model 字段：不干预
	if model == "" {
		resp, serr := send(w, r, body)
		if serr != nil {
			http.Error(w, serr.Error(), http.StatusBadGateway)
			return
		}
		relay(w, resp)
		return
	}

	bare := stripPrefix(model)
	var order []string

	if hasPrefix(model) {
		// 显式点名了域：尊重它作为首选，但**失败仍然改道另一域**。
		// 为什么：客户端常常是从 /v1/models 或面板里抄下来的 "global:xxx"，一旦
		// global 全部限流/冷却，原样透传就变成把 429/503 直接甩给调用方——
		// 期望行为是「国际版优先，国际版撑不住时用国内版」，而不是硬失败。
		first := *preferDomain
		if strings.HasPrefix(model, "cn:") {
			first = "cn"
		} else if strings.HasPrefix(model, "global:") {
			first = "global"
		}
		other := *altDomain
		if first == *altDomain {
			other = *preferDomain
		}
		if other == first {
			other = "cn"
			if first == "cn" {
				other = "global"
			}
		}
		order = []string{first, other}
		// 首选域刚被证明对这个模型不可用 → 先走另一域，省一次失败请求
		if penalties.blocked(first + ":" + bare) {
			order = []string{other, first}
		}
	} else {
		order = []string{*preferDomain, *altDomain}
		// 首选域此前已被证明没有这个模型 → 直接先走备选
		if penalties.blocked(*preferDomain + ":" + bare) {
			order = []string{*altDomain, *preferDomain}
		}
	}

	for i, domain := range order {
		full := domain + ":" + bare
		nb, rerr := rewriteModel(body, full)
		if rerr != nil {
			http.Error(w, "rewrite model: "+rerr.Error(), http.StatusInternalServerError)
			return
		}

		resp, serr := send(w, r, nb)
		if serr != nil {
			if i == len(order)-1 {
				http.Error(w, "upstream unreachable: "+serr.Error(), http.StatusBadGateway)
				return
			}
			if *verbose {
				log.Printf("[router] %s %s -> dial error, trying next", bare, domain)
			}
			continue
		}

		// HTTP 200 也可能是 SSE 错误；提交响应前先检查首个有效事件。
		if resp.StatusCode == http.StatusOK {
			retry, streamErr := inspectStream(resp)
			if r.Context().Err() != nil {
				resp.Body.Close()
				return
			}
			if (retry || streamErr != nil) && i < len(order)-1 {
				resp.Body.Close()
				penalties.mark(full, 2*time.Minute)
				if *verbose {
					log.Printf("[router] %s -> %s stream failed before output, falling back to %s", bare, domain, order[i+1])
				}
				continue
			}
			if *verbose {
				log.Printf("[router] %s -> %s (200, acct=%s)", bare, domain, resp.Header.Get("X-Wb-Account"))
			}
			relay(w, resp)
			return
		}

		// 失败：先读完错误体，判断能不能换域
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()

		last := i == len(order)-1
		if !last && shouldFallback(resp.StatusCode, errBody) {
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
		copyResponseHeaders(w, resp)
		w.Header().Set("Content-Length", itoa(len(errBody)))
		w.WriteHeader(resp.StatusCode)
		w.Write(errBody)
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

// /v1/models：把 cn:/global: 两域合并成裸名列表（同名只留一条），
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
		w.Write(raw)
		return
	}

	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		copyResponseHeaders(w, resp)
		w.Header().Set("Content-Length", itoa(len(raw)))
		w.WriteHeader(resp.StatusCode)
		w.Write(raw)
		return
	}

	var items []map[string]json.RawMessage
	if err := json.Unmarshal(top["data"], &items); err != nil {
		copyResponseHeaders(w, resp)
		w.Header().Set("Content-Length", itoa(len(raw)))
		w.WriteHeader(resp.StatusCode)
		w.Write(raw)
		return
	}

	seen := make(map[string]bool, len(items))
	out := make([]map[string]json.RawMessage, 0, len(items))
	for _, it := range items {
		var id string
		if err := json.Unmarshal(it["id"], &id); err != nil {
			continue
		}
		bare := stripPrefix(id)
		if bare == "" || seen[bare] {
			continue
		}
		seen[bare] = true
		nid, _ := json.Marshal(bare)
		it["id"] = nid
		out = append(out, it)
	}

	newData, _ := json.Marshal(out)
	top["data"] = newData
	final, _ := json.Marshal(top)

	copyResponseHeaders(w, resp)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", itoa(len(final)))
	w.WriteHeader(http.StatusOK)
	w.Write(final)
}

func handler(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/v1/models" && r.Method == http.MethodGet {
		proxyModels(w, r)
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
	log.Printf("wb-router listening on %s -> %s (裸名先走 %s，失败改道 %s)",
		*listenAddr, *upstreamAddr, *preferDomain, *altDomain)

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
