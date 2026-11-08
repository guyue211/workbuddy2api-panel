package server

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/misakano7545/workbuddy2api-panel/internal/auth"
	"github.com/misakano7545/workbuddy2api-panel/internal/upstream"
)

const highClientBody = `{"model":"cn:deepseek-v4.1-flash","max_tokens":2048,"reasoning_effort":"high","messages":[{"role":"user","content":"hi"}]}`

// TestBudgetWasRaisedDetection 判定「网关抬过额度」：客户端原始额度低于本轮下限。
func TestBudgetWasRaisedDetection(t *testing.T) {
	cases := []struct {
		name  string
		body  string
		floor int64
		want  bool
	}{
		{"high 客户端 2048 / 下限 8192", highClientBody, upstream.MinOutputBudget("high"), true},
		{"客户端 65536 / 下限 8192", `{"model":"m","max_tokens":65536}`, upstream.MinOutputBudget("high"), false},
		{"没给额度（目录补的，不算抬）", `{"model":"m"}`, upstream.MinOutputBudget("high"), false},
		{"没有下限（档位未知）", highClientBody, 0, false},
		{"别名 max_completion_tokens", `{"model":"m","max_completion_tokens":1024}`, upstream.MinOutputBudget("max"), true},
	}
	for _, c := range cases {
		r := &continueReader{body: []byte(c.body), budgetFloor: c.floor}
		if got := r.budgetWasRaised(); got != c.want {
			t.Errorf("%s: budgetWasRaised=%v want %v", c.name, got, c.want)
		}
	}
}

// TestContinueAllowedWhenBudgetRaised 网关抬过额度后，finish=length 必须续写。
//
// 线上矩阵实测（2026-11-08，effort=high、客户端 max_tokens=2048 → 抬到 8192）：
// 首段把 8192 用尽、正文 4942 token 处断句，finish=length。此时生效的上限是网关选的
// 8192，不是客户端的 2048——不续写等于「抬了额度还是断」，只是把断点从 2048 挪到 8192。
func TestContinueAllowedWhenBudgetRaised(t *testing.T) {
	up, reqs := fakeContinueUpstream(t, func(call int) (int, string) { return 200, testSeg2Text })
	r := newContinueReaderWithFloor(context.Background(), up,
		&auth.Auth{UID: "u1", AccessToken: "at-1", ExpiresAt: 9999999999},
		[]byte(highClientBody), "1.2.3.4", upstream.ChatMeta{},
		io.NopCloser(strings.NewReader(testSeg1Text)), upstream.MinOutputBudget("high"))
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(*reqs) != 1 {
		t.Fatalf("续写请求数=%d want 1（抬过额度后必须续接）", len(*reqs))
	}
	var body map[string]any
	if err := json.Unmarshal([]byte((*reqs)[0]), &body); err != nil {
		t.Fatalf("续写请求体非法: %v", err)
	}
	// 续写段：上限不缩小（至少下限），且关思考避免再被额度卡住。
	if got, _ := body["max_tokens"].(float64); int64(got) < 8192 {
		t.Fatalf("续写段 max_tokens=%v want >= 8192", body["max_tokens"])
	}
	if th, _ := body["thinking"].(map[string]any); th == nil || th["type"] != "disabled" {
		t.Fatalf("抬过额度的续写段应关思考，实际 thinking=%v", body["thinking"])
	}
	if !strings.Contains(string(out), "继续") {
		t.Fatalf("客户端应看到续接的正文:\n%s", out)
	}
}

// TestContinueStillBlockedForSmallClientLimit 客户端额度明显小于上游单段上限时，
// finish=length 是它的预期语义（"我只要 8192"），不续写。
//
// 与 TestContinueAllowedWhenClientLimitBeyondSegmentCap 是一对：判据不是「有没有
// 限额」，而是「这个限额一段装不装得下」。
func TestContinueStillBlockedForSmallClientLimit(t *testing.T) {
	up, reqs := fakeContinueUpstream(t, func(call int) (int, string) { return 200, testSeg2Text })
	r := newContinueReaderWithFloor(context.Background(), up,
		&auth.Auth{UID: "u1", AccessToken: "at-1", ExpiresAt: 9999999999},
		[]byte(`{"model":"cn:deepseek-v4.1-flash","max_tokens":8192,"reasoning_effort":"high","messages":[{"role":"user","content":"hi"}]}`),
		"1.2.3.4", upstream.ChatMeta{},
		io.NopCloser(strings.NewReader(testSeg1Text)), upstream.MinOutputBudget("high"))
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(*reqs) != 0 {
		t.Fatalf("续写请求数=%d want 0（客户端自己要的长度不续）", len(*reqs))
	}
	if !strings.Contains(string(out), `"finish_reason":"length"`) {
		t.Fatalf("length 终态应原样下发:\n%s", out)
	}
}

// TestContinueAllowedWhenClientLimitBeyondSegmentCap 客户端额度 ≥ 上游单段上限时
// 必须续写：这个数一段根本装不下，length 是上游的硬上限切出来的，不是客户端的意愿。
//
// 线上实测（2026-11-08，global:deepseek-v4.1-flash-sg，max_tokens=32000，要求
// "从 1 数到 20000"）：completion=32000 整、finish=length、正文 47853 字符停在
// "9792," 之后——网关此前按「客户端显式限额」直接收尾，用户看到的就是半路截断。
// 目录里这个模型的 max_output_tokens 是 384000（/v1/models 原样对外），任何照着
// 它填 max_tokens 的客户端都会落进这一档。
func TestContinueAllowedWhenClientLimitBeyondSegmentCap(t *testing.T) {
	for _, limit := range []int64{32000, 65536, 384000} {
		up, reqs := fakeContinueUpstream(t, func(call int) (int, string) { return 200, testSeg2Text })
		body := `{"model":"cn:deepseek-v4.1-flash","max_tokens":` + itoa(limit) + `,"reasoning_effort":"high","messages":[{"role":"user","content":"hi"}]}`
		r := newContinueReaderWithFloor(context.Background(), up,
			&auth.Auth{UID: "u1", AccessToken: "at-1", ExpiresAt: 9999999999},
			[]byte(body), "1.2.3.4", upstream.ChatMeta{},
			io.NopCloser(strings.NewReader(testSeg1Text)), upstream.MinOutputBudget("high"))
		out, err := io.ReadAll(r)
		if err != nil {
			t.Fatalf("limit=%d read: %v", limit, err)
		}
		if len(*reqs) != 1 {
			t.Fatalf("limit=%d 续写请求数=%d want 1（额度一段装不下，必须续接）", limit, len(*reqs))
		}
		s := string(out)
		if !strings.Contains(s, "继续") || !strings.Contains(s, `"finish_reason":"stop"`) {
			t.Fatalf("limit=%d 客户端应看到续接后的完整收尾:\n%s", limit, s)
		}
		if strings.Contains(s, `"finish_reason":"length"`) {
			t.Fatalf("limit=%d 续写成功后 length 不应外泄:\n%s", limit, s)
		}
	}
}

// TestOutputCappedByClientTextBudget 续写到客户端额度（按正文 token 计）就停：
// 不能因为「额度一段装不下」就无限续下去；思考不占客户端的这份预算。
func TestOutputCappedByClientTextBudget(t *testing.T) {
	cases := []struct {
		name   string
		comp   float64
		think  float64
		expect bool
	}{
		{"正文 37000 ≥ 额度 32000", 40000, 3000, true},
		{"正文恰好 32000 = 额度", 35000, 3000, true},
		{"正文 28000 < 额度 32000（思考 12000 不算进客户预算）", 40000, 12000, false},
		{"正文 58000 ≥ 额度", 70000, 12000, true},
	}
	for _, c := range cases {
		r := &continueReader{limitSet: true, clientLimit: 32000, budgetFloor: upstream.MinOutputBudget("max")}
		r.usage = map[string]any{
			"completion_tokens":          c.comp,
			"completion_thinking_tokens": c.think,
		}
		if got := r.outputCapped(); got != c.expect {
			t.Errorf("%s: outputCapped=%v want %v（text=%v）", c.name, got, c.expect, r.totalText())
		}
	}
}

func itoa(v int64) string {
	if v == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for v > 0 {
		i--
		b[i] = byte('0' + v%10)
		v /= 10
	}
	return string(b[i:])
}

// TestContinueCappedNotFlaggedOnNormalStop 正常收尾（finish=stop）即使累计输出
// 已经到顶，也不能标成「按截断收尾」——日志与终态归因会误导排障。
//
// 影子实例实测（2026-11-08，客户端 max_tokens=32000，数到 12000）：续写段自然
// 写完后仍走进 outputCapped 分支，日志打出「按截断收尾」，而客户端看到的终态是
// stop——同一份事实两个说法。
func TestContinueCappedNotFlaggedOnNormalStop(t *testing.T) {
	r := &continueReader{limitSet: true, clientLimit: 32000, budgetFloor: upstream.MinOutputBudget("max"), finish: "stop"}
	r.usage = map[string]any{"completion_tokens": float64(40000), "completion_thinking_tokens": float64(2000)}
	if r.canContinue(true) {
		t.Fatalf("已到顶且正常收尾，不该续写")
	}
	if r.continueCapped {
		t.Fatalf("正常收尾不该标成「按截断收尾」")
	}
	// 同样是到顶，但本段是 length 截断：这才是真截断，必须标出来。
	r2 := &continueReader{limitSet: true, clientLimit: 32000, budgetFloor: upstream.MinOutputBudget("max"), finish: "length"}
	r2.usage = map[string]any{"completion_tokens": float64(40000), "completion_thinking_tokens": float64(2000)}
	if r2.canContinue(true) {
		t.Fatalf("已到顶不该续写")
	}
	if !r2.continueCapped {
		t.Fatalf("length 截断且到顶，必须标成按截断收尾")
	}
}
