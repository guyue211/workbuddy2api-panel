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
}
