package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
)

// 只预读首个有效输出前的事件；正文/工具调用开始后绝不重试。
// 原始字节完整放回，正常流不改写、不聚合。
func inspectStream(resp *http.Response) (bool, error) {
	if !strings.HasPrefix(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream") {
		return false, nil
	}
	original := resp.Body
	reader := bufio.NewReader(original)
	var prefix bytes.Buffer
	var event, data string
	defer func() {
		resp.Body = &replayBody{Reader: io.MultiReader(bytes.NewReader(prefix.Bytes()), reader), closer: original}
	}()
	for prefix.Len() < 256<<10 {
		line, err := reader.ReadSlice('\n')
		prefix.Write(line)
		if err == bufio.ErrBufferFull {
			return false, nil
		}
		text := strings.TrimSuffix(strings.TrimSuffix(string(line), "\n"), "\r")
		if text == "" {
			if data != "" {
				retry, wait := inspectEvent(event, []byte(data))
				if retry || !wait {
					return retry, nil
				}
			}
			event, data = "", ""
		} else if strings.HasPrefix(text, "event:") {
			event = strings.TrimSpace(strings.TrimPrefix(text, "event:"))
		} else if strings.HasPrefix(text, "data:") {
			if data != "" {
				data += "\n"
			}
			data += strings.TrimPrefix(strings.TrimPrefix(text, "data:"), " ")
		}
		if err != nil {
			if err == io.EOF {
				if data != "" {
					retry, wait := inspectEvent(event, []byte(data))
					if retry || !wait {
						return retry, nil
					}
				}
				return true, nil // 没有有效输出便断流，可以安全换域。
			}
			return false, err
		}
	}
	return false, nil
}

type replayBody struct {
	io.Reader
	closer io.Closer
}

func (r *replayBody) Close() error { return r.closer.Close() }

func inspectEvent(event string, data []byte) (retry, wait bool) {
	var m map[string]json.RawMessage
	if json.Unmarshal(data, &m) != nil {
		return false, false
	}
	var typ string
	json.Unmarshal(m["type"], &typ)
	if typ == "" {
		typ = event
	}
	rawError := m["error"]
	if typ == "response.failed" {
		var response map[string]json.RawMessage
		json.Unmarshal(m["response"], &response)
		rawError = response["error"]
	}
	if len(rawError) > 0 && string(rawError) != "null" {
		var e struct {
			Code json.RawMessage `json:"code"`
			Type string          `json:"type"`
		}
		json.Unmarshal(rawError, &e)
		code := strings.Trim(string(e.Code), "\"")
		// 账号受限可能被网关包成 no_healthy_account；保留限制保护，不跨区重试。
		if code == "account_restricted_hold" || bytes.Contains(rawError, []byte("11140")) {
			return false, false
		}
		switch e.Type {
		case "insufficient_quota", "rate_limit_error", "rate_limit_exceeded", "overloaded_error", "server_error", "model_unavailable":
			return true, false
		}
		switch code {
		case "402", "429", "500", "502", "503", "504", "6004", "rate_limit_exceeded", "insufficient_quota", "model_unavailable", "no_healthy_account":
			return true, false
		}
		return false, false // 鉴权、参数等错误不能靠换域修复。
	}
	switch typ {
	case "response.created", "response.in_progress", "ping", "message_start":
		return false, true
	case "content_block_start":
		var block struct {
			Type string `json:"type"`
		}
		json.Unmarshal(m["content_block"], &block)
		return false, block.Type == "text" || block.Type == "thinking"
	}
	if choices, ok := m["choices"]; ok {
		var cs []struct {
			Delta  map[string]json.RawMessage `json:"delta"`
			Finish *string                    `json:"finish_reason"`
		}
		if json.Unmarshal(choices, &cs) == nil && len(cs) > 0 {
			for _, c := range cs {
				if c.Finish != nil {
					return false, false
				}
				for k, v := range c.Delta {
					if k != "role" && string(v) != "null" && string(v) != "\"\"" {
						return false, false
					}
				}
			}
			return false, true
		}
	}
	return false, false
}
