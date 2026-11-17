package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/misakano7545/workbuddy2api-panel/internal/auth"
	"github.com/misakano7545/workbuddy2api-panel/internal/session"
)

// 原模型 global:deepseek-v4.1-flash 已在白名单时，内置强制变体 -sg 不得被二次白名单检查误拒。
func TestForcedDeepseekSgAliasAllowedWhenSourceInAllowlist(t *testing.T) {
	h, sent := globalAliasTestHandler(t, nil, nil, []string{"global:deepseek-v4.1-flash"})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"global:deepseek-v4.1-flash","messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s, want 200", rec.Code, rec.Body)
	}
	var out map[string]any
	_ = json.Unmarshal(*sent, &out)
	if got := out["model"]; got != "deepseek-v4.1-flash-sg" {
		t.Fatalf("outbound model=%v, want deepseek-v4.1-flash-sg", got)
	}
}

// 开启会话粘性时，新会话首轮必须走 Pool.Pick 完整调度管线（尊重 account_priority），不能按哈希乱绑非优先号。
func TestNewSessionStickyHonorsAccountPriority(t *testing.T) {
	p := testPoolWith(
		&auth.Auth{UID: "low-1", AccessToken: "at-low1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "low-2", AccessToken: "at-low2", ExpiresAt: 9999999999},
		&auth.Auth{UID: "prio-1", AccessToken: "at-prio1", ExpiresAt: 9999999999},
	)
	p.SetAccountPriority(map[string]int{"prio-1": 1})

	st := newBindStore()
	sess := session.New(session.Config{
		TTL:       time.Minute,
		Store:     st,
		Available: p.AvailableUIDs,
	})
	var seenAuths []string
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		seenAuths = append(seenAuths, authz)
		return 200, sseOK, true
	})
	h := NewHandler(Config{Pool: p, Upstream: up, Session: sess, SoftCooldown: time.Minute})

	for i := 0; i < 6; i++ {
		body := fmt.Sprintf(`{"model":"glm-5.2","conversation_id":"conv-%d","messages":[{"role":"user","content":"hi"}]}`, i)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body)))
		if rec.Code != http.StatusOK {
			t.Fatalf("req %d status=%d body=%s", i, rec.Code, rec.Body)
		}
	}
	for i, a := range seenAuths {
		if a != "Bearer at-prio1" {
			t.Fatalf("req %d picked %q instead of priority account prio-1", i, a)
		}
	}
}

// /v1/messages 流式路径必须透传 SSE 注释保活帧（: heartbeat / : keepalive）。
func TestMessagesStreamForwardsKeepaliveComments(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at-1", ExpiresAt: 9999999999})
	sseWithHeartbeat := ": heartbeat\n\ndata: {\"id\":\"1\",\"choices\":[{\"delta\":{\"content\":\"pong\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		return 200, sseWithHeartbeat, true
	})
	h := NewHandler(Config{Pool: p, Upstream: up})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/messages",
		strings.NewReader(`{"model":"glm-5.2","stream":true,"max_tokens":64,"messages":[{"role":"user","content":"ping"}]}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), ": heartbeat\n\n") {
		t.Fatalf("expected SSE comment frame to be forwarded on /v1/messages, got:\n%s", rec.Body.String())
	}
}

// HTTP 200 + 首帧 error 触发首帧否决换号时，不得在否决前误调 NoteSuccess 清零失败计数。
func TestHeadVetoDoesNotClearBreakerFails(t *testing.T) {
	p := testPoolWith(
		&auth.Auth{UID: "bad", AccessToken: "at-bad", ExpiresAt: 9999999999},
		&auth.Auth{UID: "good", AccessToken: "at-good", ExpiresAt: 9999999999},
	)
	p.SetCredits("bad", 5000, 5000)
	p.SetCredits("good", 100, 100)
	p.NoteError("bad") // 已有 1 次失败

	sseErrFirstFrame := "data: {\"error\":{\"code\":500,\"message\":\"internal server error\"}}\n\ndata: [DONE]\n\n"
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		if authz == "Bearer at-bad" {
			return 200, sseErrFirstFrame, true
		}
		return 200, sseOK, true
	})
	h := NewHandler(Config{Pool: p, Upstream: up, MaxRotate: 3, SoftCooldown: time.Minute})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body)
	}
	st, _ := p.Status("bad")
	if st.BreakerFails != 1 || st.ConsecutiveFails < 1 {
		t.Fatalf("HeadVeto must not wipe prior BreakerFails via premature NoteSuccess: got BreakerFails=%d ConsecutiveFails=%d", st.BreakerFails, st.ConsecutiveFails)
	}
}
