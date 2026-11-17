package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestStreamFallback(t *testing.T) {
	cases := []struct {
		name, path, first string
		fallback          bool
	}{
		{"responses quota", "/v1/responses", "event: response.failed\ndata: {\"type\":\"response.failed\",\"response\":{\"error\":{\"code\":6004,\"type\":\"insufficient_quota\"}}}\n\n", true},
		{"responses pool empty", "/v1/responses", "event: response.failed\ndata: {\"type\":\"response.failed\",\"response\":{\"error\":{\"code\":\"no_healthy_account\",\"type\":\"api_error\"}}}\n\n", true},
		{"chat pool empty", "/v1/chat/completions", ": heartbeat\n\ndata: {\"error\":{\"code\":\"no_healthy_account\",\"type\":\"api_error\"}}\n\n", true},
		{"restriction hold stays local", "/v1/responses", "event: response.failed\ndata: {\"type\":\"response.failed\",\"response\":{\"error\":{\"code\":\"account_restricted_hold\",\"type\":\"api_error\"}}}\n\n", false},
		{"heartbeat then error", "/v1/chat/completions", ": heartbeat\n\ndata: {\"error\":{\"type\":\"rate_limit_error\"}}\n\n", true},
		{"init then failure", "/v1/responses", "event: response.created\ndata: {\"type\":\"response.created\"}\n\nevent: response.failed\ndata: {\"response\":{\"error\":{\"type\":\"server_error\"}}}\n\n", true},
		{"messages overload", "/v1/messages", "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\"}}\n\n", true},
		{"auth not retried", "/messages", "event: error\ndata: {\"error\":{\"type\":\"authentication_error\"}}\n\n", false},
		{"output never retried", "/v1/chat/completions", "data: {\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\ndata: {\"error\":{\"type\":\"rate_limit_error\"}}\n\n", false},
		{"tool never retried", "/v1/messages", "data: {\"type\":\"content_block_start\",\"content_block\":{\"type\":\"tool_use\",\"id\":\"x\"}}\n\ndata: {\"error\":{\"type\":\"overloaded_error\"}}\n\n", false},
		{"role then failure", "/v1/chat/completions", "data: {\"choices\":[{\"delta\":{\"role\":\"assistant\"}}]}\n\ndata: {\"error\":{\"code\":429}}\n\n", true},
		{"heartbeat EOF", "/v1/responses", ": heartbeat\n\n", true},
		{"empty EOF", "/v1/responses", "", true},
		{"CRLF failure", "/v1/responses", "event: response.failed\r\ndata: {\"response\":{\"error\":{\"code\":6004}}}\r\n\r\n", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			penalties = newPenaltyTable()
			realmHealth = newRealmHealthState()
			var models []string
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body struct {
					Model string `json:"model"`
				}
				json.NewDecoder(r.Body).Decode(&body)
				models = append(models, body.Model)
				w.Header().Set("Content-Type", "text/event-stream")
				if strings.HasPrefix(body.Model, "global:") {
					io.WriteString(w, tc.first)
				} else {
					io.WriteString(w, "data: domestic-success\n\n")
				}
			}))
			defer upstream.Close()
			old := *upstreamAddr
			*upstreamAddr = strings.TrimPrefix(upstream.URL, "http://")
			defer func() { *upstreamAddr = old }()
			req := httptest.NewRequest("POST", tc.path, strings.NewReader(`{"model":"global:deepseek-v4.1-flash","stream":true}`))
			rec := httptest.NewRecorder()
			handler(rec, req)
			want := tc.first
			if tc.fallback {
				want = "data: domestic-success\n\n"
				if len(models) != 2 || models[1] != "cn:deepseek-v4.1-flash" {
					t.Fatalf("models %v", models)
				}
			} else if len(models) != 1 {
				t.Fatalf("unexpected retry %v", models)
			}
			if rec.Body.String() != want {
				t.Fatalf("got %q want %q", rec.Body.String(), want)
			}
			if tc.fallback {
				rec = httptest.NewRecorder()
				handler(rec, httptest.NewRequest("POST", tc.path, strings.NewReader(`{"model":"global:deepseek-v4.1-flash","stream":true}`)))
				if len(models) != 3 || models[2] != "cn:deepseek-v4.1-flash" {
					t.Fatalf("penalty ignored %v", models)
				}
			}
		})
	}
}

func TestStreamReplay(t *testing.T) {
	for _, s := range []string{"data: [DONE]\n\n", "data: " + strings.Repeat("x", 10000) + "\n\n", "data: {\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\ntrailing"} {
		resp := &http.Response{Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(s))}
		retry, err := inspectStream(resp)
		if retry || err != nil {
			t.Fatalf("unexpected retry %v %v", retry, err)
		}
		b, _ := io.ReadAll(resp.Body)
		if string(b) != s {
			t.Fatal("stream changed")
		}
	}
}

func TestHTTPFallback(t *testing.T) {
	penalties = newPenaltyTable()
	realmHealth = newRealmHealthState()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if strings.Contains(string(b), "global:") {
			w.WriteHeader(429)
		} else {
			io.WriteString(w, `{"ok":true}`)
		}
	}))
	defer upstream.Close()
	old := *upstreamAddr
	*upstreamAddr = strings.TrimPrefix(upstream.URL, "http://")
	defer func() { *upstreamAddr = old }()
	rec := httptest.NewRecorder()
	handler(rec, httptest.NewRequest("POST", "/v1/responses", strings.NewReader(`{"model":"deepseek"}`)))
	if rec.Code != 200 || rec.Body.String() != `{"ok":true}` {
		t.Fatal(rec.Code, rec.Body.String())
	}
	if rec.Header().Get("X-Wb-Router-Domain") != "cn" || rec.Header().Get("X-Wb-Router-Fallback") != "1" {
		t.Fatalf("unexpected router headers: %v", rec.Header())
	}
}

func TestLargeResponseCreatedLineStillFallsBackOnFailed(t *testing.T) {
	penalties = newPenaltyTable()
	realmHealth = newRealmHealthState()
	hugeInstructions := strings.Repeat("A", 24<<10) // > 16KB 单行，跨越 bufio 默认与扩增缓冲边界
	firstStream := "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"instructions\":\"" + hugeInstructions + "\"}}\n\n" +
		"event: response.failed\ndata: {\"type\":\"response.failed\",\"response\":{\"error\":{\"code\":6004,\"type\":\"insufficient_quota\"}}}\n\n"

	var models []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		models = append(models, body.Model)
		w.Header().Set("Content-Type", "text/event-stream")
		if strings.HasPrefix(body.Model, "global:") {
			_, _ = io.WriteString(w, firstStream)
		} else {
			_, _ = io.WriteString(w, "data: cn-rescued\n\n")
		}
	}))
	defer upstream.Close()
	old := *upstreamAddr
	*upstreamAddr = strings.TrimPrefix(upstream.URL, "http://")
	defer func() { *upstreamAddr = old }()

	rec := httptest.NewRecorder()
	handler(rec, httptest.NewRequest("POST", "/v1/responses", strings.NewReader(`{"model":"deepseek-v4.1-flash","stream":true}`)))
	if len(models) != 2 || models[0] != "global:deepseek-v4.1-flash" || models[1] != "cn:deepseek-v4.1-flash" {
		t.Fatalf("expected fallback after >16KB response.created, got models=%v", models)
	}
	if rec.Body.String() != "data: cn-rescued\n\n" {
		t.Fatalf("got %q", rec.Body.String())
	}
}

func TestRealmHealthSkipsUnservablePreferDomain(t *testing.T) {
	penalties = newPenaltyTable()
	realmHealth = newRealmHealthState()

	var models []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			_, _ = io.WriteString(w, `{"healthy":9,"realm_servable":{"cn":true,"global":false},"service":"workbuddy2api","total":12}`)
			return
		}
		if r.URL.Path == "/v1/models" {
			_, _ = io.WriteString(w, `{"object":"list","data":[
				{"id":"cn:deepseek-v4.1-flash","max_output_tokens":384000,"credits":"x0.50"},
				{"id":"global:deepseek-v4.1-flash","max_output_tokens":128000,"credits":"x0.00"},
				{"id":"global:gpt-5.4","max_output_tokens":64000}
			]}`)
			return
		}
		var body struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		models = append(models, body.Model)
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer upstream.Close()
	old := *upstreamAddr
	*upstreamAddr = strings.TrimPrefix(upstream.URL, "http://")
	defer func() { *upstreamAddr = old }()

	if !refreshRealmHealthOnce(*upstreamAddr) {
		t.Fatal("refreshRealmHealthOnce failed")
	}

	// global 明确不可服务时，第一跳直接走 cn，零无效探测。
	rec := httptest.NewRecorder()
	handler(rec, httptest.NewRequest("POST", "/v1/messages/count_tokens", strings.NewReader(`{"messages":[{"role":"user","content":"{\"model\":\"nested\"}"}],"model":"deepseek-v4.1-flash"}`)))
	if rec.Code != 200 || len(models) != 1 || models[0] != "cn:deepseek-v4.1-flash" {
		t.Fatalf("expected direct route to cn, got code=%d models=%v", rec.Code, models)
	}

	// /v1/models 合并时优先采用当前有效首选域（此时为 cn）的元数据，且支持 /v1/models/{id} 单查。
	mrec := httptest.NewRecorder()
	handler(mrec, httptest.NewRequest("GET", "/v1/models/deepseek-v4.1-flash", nil))
	if mrec.Code != 200 || !strings.Contains(mrec.Body.String(), `"credits":"x0.50"`) || !strings.Contains(mrec.Body.String(), `"id":"deepseek-v4.1-flash"`) {
		t.Fatalf("single model lookup unexpected: %d %s", mrec.Code, mrec.Body.String())
	}

	// /router/status 本地回环可读。
	srec := httptest.NewRecorder()
	sreq := httptest.NewRequest("GET", "/router/status", nil)
	sreq.RemoteAddr = "127.0.0.1:54321"
	handler(srec, sreq)
	if srec.Code != 200 || !strings.Contains(srec.Body.String(), `"effective_prefer":"cn"`) {
		t.Fatalf("router status unexpected: %d %s", srec.Code, srec.Body.String())
	}
}

func TestNonStreamRestrictionHoldNotRetriedAcrossRealms(t *testing.T) {
	penalties = newPenaltyTable()
	realmHealth = newRealmHealthState()
	var calls int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, `{"error":{"code":"account_restricted_hold","message":"403 / code 11140"}}`)
	}))
	defer upstream.Close()
	old := *upstreamAddr
	*upstreamAddr = strings.TrimPrefix(upstream.URL, "http://")
	defer func() { *upstreamAddr = old }()

	rec := httptest.NewRecorder()
	handler(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"deepseek-v4.1-flash"}`)))
	if rec.Code != http.StatusServiceUnavailable || calls != 1 {
		t.Fatalf("restriction hold must not retry alt realm: code=%d calls=%d", rec.Code, calls)
	}
}

func TestMaxBodyLimitReturns413InsteadOfTruncating(t *testing.T) {
	oldMax := *maxBody
	*maxBody = 64
	defer func() { *maxBody = oldMax }()

	rec := httptest.NewRecorder()
	big := `{"model":"deepseek-v4.1-flash","input":"` + strings.Repeat("x", 128) + `"}`
	handler(rec, httptest.NewRequest("POST", "/v1/responses", strings.NewReader(big)))
	if rec.Code != http.StatusRequestEntityTooLarge || !strings.Contains(rec.Body.String(), "payload_too_large") {
		t.Fatalf("expected 413 payload_too_large, got %d %s", rec.Code, rec.Body.String())
	}
}

func TestFastSplicePreservesExactKeyOrder(t *testing.T) {
	orig := `{"instructions":"keep","tools":[{"name":"a","model":"inner"}],"model":"deepseek-v4.1-flash","stream":true}`
	model, vs, ve, ok := findTopLevelModel([]byte(orig))
	if !ok || model != "deepseek-v4.1-flash" {
		t.Fatalf("findTopLevelModel failed: ok=%v model=%q", ok, model)
	}
	got, err := spliceModel([]byte(orig), vs, ve, "global:deepseek-v4.1-flash")
	if err != nil {
		t.Fatal(err)
	}
	want := `{"instructions":"keep","tools":[{"name":"a","model":"inner"}],"model":"global:deepseek-v4.1-flash","stream":true}`
	if string(got) != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
}
