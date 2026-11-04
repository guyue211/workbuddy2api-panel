package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/misakano7545/workbuddy2api-panel/internal/auth"
	"github.com/misakano7545/workbuddy2api-panel/internal/livecfg"
	"github.com/misakano7545/workbuddy2api-panel/internal/upstream"
)

// aliasTestHandler 构造一个带 aliases 快照的 handler，并返回出站 body 捕获口。
//
// 出站 body 由 fake transport 读 r.Body 捕获（prepareBody 已强制 stream + 归一），
// 断言点与 TestCustomModeFingerprintSanitizePreserved 同一位置。
func aliasTestHandler(t *testing.T, aliasEntries []string, hidden []string) (*Handler, *[]byte) {
	t.Helper()
	var sentBody []byte
	up := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			raw, _ := io.ReadAll(r.Body)
			sentBody = raw
			return &http.Response{
				StatusCode: 200,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(strings.NewReader(sseOK)),
			}, nil
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999}),
		Upstream: up,
		Live: livecfg.New(livecfg.Snapshot{
			HiddenModels: livecfg.NormalizeHiddenModels(hidden),
			ModelAliases: livecfg.NormalizeModelAliases(aliasEntries),
		}),
	})
	return h, &sentBody
}

// TestModelAliasRewritesOutboundBody 映射命中：出站 body 的 model 换成真实名，
// 响应（聚合体）的 model 回显客户端原发名（issue #30）。
func TestModelAliasRewritesOutboundBody(t *testing.T) {
	h, sent := aliasTestHandler(t, []string{"my-gpt=glm-5.2"}, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"my-gpt","messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body)
	}
	var out map[string]any
	if err := json.Unmarshal(*sent, &out); err != nil {
		t.Fatalf("出站 body 解析失败: %v (%s)", err, *sent)
	}
	if got := out["model"]; got != "glm-5.2" {
		t.Errorf("出站 model = %v, want glm-5.2（映射后真实名）", got)
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("响应解析失败: %v (%s)", err, rec.Body)
	}
	if got := resp["model"]; got != "my-gpt" {
		t.Errorf("响应 model = %v, want my-gpt（回显客户端原发名）", got)
	}
}

// TestModelAliasStreamEcho 流式：帧内 model 覆写回客户端原发名。
func TestModelAliasStreamEcho(t *testing.T) {
	h, sent := aliasTestHandler(t, []string{"my-gpt=glm-5.2"}, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"my-gpt","stream":true,"messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body)
	}
	var out map[string]any
	_ = json.Unmarshal(*sent, &out)
	if got := out["model"]; got != "glm-5.2" {
		t.Errorf("出站 model = %v, want glm-5.2", got)
	}
	// 上游帧里 model 是 glm-5.2（sseOK 固定值），出站帧必须已改回 my-gpt。
	body := rec.Body.String()
	if strings.Contains(body, `"model":"glm-5.2"`) {
		t.Errorf("流式帧仍带真实模型名，回显未生效:\n%s", body)
	}
	if !strings.Contains(body, `"model":"my-gpt"`) {
		t.Errorf("流式帧缺客户端原发名:\n%s", body)
	}
}

// TestModelAliasCannotBypassHidden 别名目标受黑名单约束（映射在 hidden 检查之前
// 完成，检查的是真实名）。
func TestModelAliasCannotBypassHidden(t *testing.T) {
	h, sent := aliasTestHandler(t, []string{"my-gpt=glm-5.2"}, []string{"glm-5.2"})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"my-gpt","messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s, want 400（映射目标被隐藏）", rec.Code, rec.Body)
	}
	if len(*sent) != 0 {
		t.Errorf("被隐藏的映射目标不应打上游，实际出站 body=%s", *sent)
	}
}

// TestModelAliasLookup 查找口径：大小写不敏感；域前缀键只在该域生效；
// 未配置的表（nil）不命中。
func TestModelAliasLookup(t *testing.T) {
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999}),
		Upstream: newFakeUpstream(t, func(string) (int, string, bool) { return 200, sseOK, true }),
		Live: livecfg.New(livecfg.Snapshot{
			ModelAliases: livecfg.NormalizeModelAliases([]string{"My-GPT=glm-5.2", "cn:opus=global:gpt-5.4"}),
		}),
	})
	if got, ok := h.modelAlias("cn", "my-gpt"); !ok || got != "glm-5.2" {
		t.Errorf("大小写不敏感查找失败: %q ok=%v", got, ok)
	}
	if got, ok := h.modelAlias("CN", "MY-GPT"); !ok || got != "glm-5.2" {
		t.Errorf("realm 大写归一化失败: %q ok=%v", got, ok)
	}
	if _, ok := h.modelAlias("global", "opus"); ok {
		t.Error("cn: 前缀键不应在 global 域命中")
	}
	if got, ok := h.modelAlias("cn", "opus"); !ok || got != "global:gpt-5.4" {
		t.Errorf("跨域指向值应原样返回: %q ok=%v", got, ok)
	}
	if _, ok := h.modelAlias("cn", ""); ok {
		t.Error("空模型名不应命中")
	}

	// 无表（nil 快照）→ 不命中
	h2 := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999}),
		Upstream: newFakeUpstream(t, func(string) (int, string, bool) { return 200, sseOK, true }),
	})
	if _, ok := h2.modelAlias("cn", "my-gpt"); ok {
		t.Error("未配置映射时不应命中")
	}
}

// globalAliasTestHandler 构造一个同时有 cn/global 账号的 handler，出站 body 捕获口
// 同 aliasTestHandler；allowed 非空即启用白名单。国际版 base 也指向 fake，
// 强制映射用例能走通 global 选号而不触网。
func globalAliasTestHandler(t *testing.T, aliasEntries, hidden, allowed []string) (*Handler, *[]byte) {
	t.Helper()
	var sentBody []byte
	up := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			raw, _ := io.ReadAll(r.Body)
			sentBody = raw
			return &http.Response{
				StatusCode: 200,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(strings.NewReader(sseOK)),
			}, nil
		})},
		ChatBaseCN:        "https://fake.example",
		BillingBaseCN:     "https://fake.example",
		ChatBaseGlobal:    "https://fake.example",
		BillingBaseGlobal: "https://fake.example",
	}
	allowedModels, allowlistEnabled := livecfg.NormalizeAllowedModels(allowed)
	h := NewHandler(Config{
		Pool: testPoolWith(
			&auth.Auth{UID: "cn1", Domain: "www.codebuddy.cn", AccessToken: "at-cn", ExpiresAt: 9999999999},
			&auth.Auth{UID: "g1", Domain: "www.workbuddy.ai", AccessToken: "at-global", ExpiresAt: 9999999999},
		),
		Upstream: up,
		Live: livecfg.New(livecfg.Snapshot{
			HiddenModels:     livecfg.NormalizeHiddenModels(hidden),
			ModelAliases:     livecfg.NormalizeModelAliases(aliasEntries),
			AllowedModels:    allowedModels,
			AllowlistEnabled: allowlistEnabled,
		}),
	})
	return h, &sentBody
}

// TestForcedDeepseekSgAliasGlobal INTL 的 deepseek-v4.1-flash 强制走付费变体 -sg：
// 出站 model 换成 deepseek-v4.1-flash-sg（realm 前缀被 prepareBody 剥掉），
// 响应仍回显客户端原发名（含 global: 前缀）。
func TestForcedDeepseekSgAliasGlobal(t *testing.T) {
	h, sent := globalAliasTestHandler(t, nil, nil, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"global:deepseek-v4.1-flash","messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body)
	}
	var out map[string]any
	if err := json.Unmarshal(*sent, &out); err != nil {
		t.Fatalf("出站 body 解析失败: %v (%s)", err, *sent)
	}
	if got := out["model"]; got != "deepseek-v4.1-flash-sg" {
		t.Errorf("出站 model = %v, want deepseek-v4.1-flash-sg（强制付费变体）", got)
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("响应解析失败: %v (%s)", err, rec.Body)
	}
	if got := resp["model"]; got != "global:deepseek-v4.1-flash" {
		t.Errorf("响应 model = %v, want global:deepseek-v4.1-flash（回显客户端原发名）", got)
	}
}

// TestForcedDeepseekSgAliasSkipsCN CN 目录没有 -sg，裸名（默认 cn 域）必须原样透传。
func TestForcedDeepseekSgAliasSkipsCN(t *testing.T) {
	h, sent := globalAliasTestHandler(t, nil, nil, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"deepseek-v4.1-flash","messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body)
	}
	var out map[string]any
	_ = json.Unmarshal(*sent, &out)
	if got := out["model"]; got != "deepseek-v4.1-flash" {
		t.Errorf("CN 出站 model = %v, want deepseek-v4.1-flash（CN 无 -sg，不得改写）", got)
	}
}

// TestForcedDeepseekSgAliasOtherModels 强制表只认 ds 一条，其他 global 模型不受影响。
func TestForcedDeepseekSgAliasOtherModels(t *testing.T) {
	h, sent := globalAliasTestHandler(t, nil, nil, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"global:glm-5.2","messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body)
	}
	var out map[string]any
	_ = json.Unmarshal(*sent, &out)
	if got := out["model"]; got != "glm-5.2" {
		t.Errorf("出站 model = %v, want glm-5.2", got)
	}
}

// TestForcedDeepseekSgAliasConfigOverride config aliases 显式配置优先于内置强制表。
func TestForcedDeepseekSgAliasConfigOverride(t *testing.T) {
	h, sent := globalAliasTestHandler(t, []string{"global:deepseek-v4.1-flash=global:glm-5.2"}, nil, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"global:deepseek-v4.1-flash","messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body)
	}
	var out map[string]any
	_ = json.Unmarshal(*sent, &out)
	if got := out["model"]; got != "glm-5.2" {
		t.Errorf("出站 model = %v, want glm-5.2（配置别名应覆盖强制表）", got)
	}
}

// TestForcedDeepseekSgAliasCannotBypassHidden 原模型被隐藏时不做强制改写，
// 后续 hidden 检查按原模型拒绝——强制表不能成为绕过黑名单的后门。
func TestForcedDeepseekSgAliasCannotBypassHidden(t *testing.T) {
	h, sent := globalAliasTestHandler(t, nil, []string{"global:deepseek-v4.1-flash"}, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"global:deepseek-v4.1-flash","messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s, want 400（原模型被隐藏）", rec.Code, rec.Body)
	}
	if len(*sent) != 0 {
		t.Errorf("被隐藏的模型不应打上游，实际出站 body=%s", *sent)
	}
}

// TestForcedDeepseekSgAliasCannotBypassAllowlist 白名单启用且原模型不在名单时
// 不做强制改写，后续白名单检查按原模型拒绝。
func TestForcedDeepseekSgAliasCannotBypassAllowlist(t *testing.T) {
	h, sent := globalAliasTestHandler(t, nil, nil, []string{"global:glm-5.2"})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"global:deepseek-v4.1-flash","messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s, want 400（原模型不在白名单）", rec.Code, rec.Body)
	}
	if len(*sent) != 0 {
		t.Errorf("白名单外的模型不应打上游，实际出站 body=%s", *sent)
	}
}

// TestModelAliasUnmappedZeroChange 未命中映射的请求：出站 body 与响应都保持原样
// （别名字段存在也不影响未映射模型）。
func TestModelAliasUnmappedZeroChange(t *testing.T) {
	h, sent := aliasTestHandler(t, []string{"my-gpt=glm-5.2"}, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body)
	}
	var out map[string]any
	_ = json.Unmarshal(*sent, &out)
	if got := out["model"]; got != "glm-5.2" {
		t.Errorf("未映射模型出站 model = %v, want glm-5.2", got)
	}
}
