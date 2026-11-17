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
	retry, err, _ := inspectStreamDetailed(resp)
	return retry, err
}

func inspectStreamDetailed(resp *http.Response) (retry bool, err error, poolEmpty bool) {
	if !strings.HasPrefix(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream") {
		return false, nil, false
	}
	original := resp.Body
	reader := bufio.NewReaderSize(original, 16<<10)
	var prefix bytes.Buffer
	var lineBuf bytes.Buffer
	var event, data string
	const maxInspectBytes = 256 << 10
	defer func() {
		resp.Body = &replayBody{Reader: io.MultiReader(bytes.NewReader(prefix.Bytes()), reader), closer: original}
	}()
	for prefix.Len() < maxInspectBytes {
		frag, err := reader.ReadSlice('\n')
		prefix.Write(frag)
		if err == bufio.ErrBufferFull {
			// 单行超过读缓冲（常见于 /v1/responses 的 response.created 回显大段 instructions/tools）：
			// 继续拼接整行直至上限，不能在 4KB/16KB 处提前放弃预检，否则紧随其后的 response.failed 无法回退。
			lineBuf.Write(frag)
			if prefix.Len() >= maxInspectBytes {
				return false, nil, false
			}
			continue
		}
		var rawLine []byte
		if lineBuf.Len() > 0 {
			lineBuf.Write(frag)
			rawLine = lineBuf.Bytes()
		} else {
			rawLine = frag
		}
		text := strings.TrimSuffix(strings.TrimSuffix(string(rawLine), "\n"), "\r")
		lineBuf.Reset()
		if text == "" {
			if data != "" {
				retry, wait, pe := inspectEventDetailed(event, []byte(data))
				if retry || !wait {
					return retry, nil, pe
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
					retry, wait, pe := inspectEventDetailed(event, []byte(data))
					if retry || !wait {
						return retry, nil, pe
					}
				}
				return true, nil, false // 没有有效输出便断流，可以安全换域。
			}
			return false, err, false
		}
	}
	return false, nil, false
}

type replayBody struct {
	io.Reader
	closer io.Closer
}

func (r *replayBody) Close() error { return r.closer.Close() }

func inspectEvent(event string, data []byte) (retry, wait bool) {
	r, w, _ := inspectEventDetailed(event, data)
	return r, w
}

func inspectEventDetailed(event string, data []byte) (retry, wait, poolEmpty bool) {
	var m map[string]json.RawMessage
	if json.Unmarshal(data, &m) != nil {
		return false, false, false
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
			return false, false, false
		}
		if code == "no_healthy_account" {
			return true, false, true
		}
		switch e.Type {
		case "insufficient_quota", "rate_limit_error", "rate_limit_exceeded", "overloaded_error", "server_error", "model_unavailable":
			return true, false, false
		}
		switch code {
		case "402", "429", "500", "502", "503", "504", "6004", "rate_limit_exceeded", "insufficient_quota", "model_unavailable":
			return true, false, false
		}
		return false, false, false // 鉴权、参数等错误不能靠换域修复。
	}
	switch typ {
	case "response.created", "response.in_progress", "ping", "message_start":
		return false, true, false
	case "content_block_start":
		var block struct {
			Type string `json:"type"`
		}
		json.Unmarshal(m["content_block"], &block)
		return false, block.Type == "text" || block.Type == "thinking", false
	}
	if choices, ok := m["choices"]; ok {
		var cs []struct {
			Delta  map[string]json.RawMessage `json:"delta"`
			Finish *string                    `json:"finish_reason"`
		}
		if json.Unmarshal(choices, &cs) == nil && len(cs) > 0 {
			for _, c := range cs {
				if c.Finish != nil {
					return false, false, false
				}
				for k, v := range c.Delta {
					if k != "role" && string(v) != "null" && string(v) != "\"\"" {
						return false, false, false
					}
				}
			}
			return false, true, false
		}
	}
	return false, false, false
}
