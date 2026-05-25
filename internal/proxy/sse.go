package proxy

import (
	"encoding/json"
	"strings"
)

// SSEReassembler parses raw SSE bytes and reconstructs the complete API response.
type SSEReassembler struct {
	Events   []map[string]interface{}
	buf      []byte
	currentEvent string
	dataLines    []string
	snapshot map[string]interface{}
}

// NewSSEReassembler creates a new SSE reassembler.
func NewSSEReassembler() *SSEReassembler {
	return &SSEReassembler{
		Events: make([]map[string]interface{}, 0),
	}
}

// FeedBytes feeds raw SSE bytes into the reassembler.
func (s *SSEReassembler) FeedBytes(chunk []byte) {
	s.buf = append(s.buf, chunk...)
	for {
		idx := indexByte(s.buf, '\n')
		if idx < 0 {
			break
		}
		line := string(s.buf[:idx])
		s.buf = s.buf[idx+1:]
		s.feedLine(strings.TrimRight(line, "\r"))
	}
}

func (s *SSEReassembler) feedLine(line string) {
	if strings.HasPrefix(line, "event:") {
		s.currentEvent = strings.TrimSpace(line[6:])
		s.dataLines = nil
	} else if strings.HasPrefix(line, "data:") {
		s.dataLines = append(s.dataLines, strings.TrimSpace(line[5:]))
	} else if line == "" {
		if s.currentEvent != "" || len(s.dataLines) > 0 {
			rawData := strings.Join(s.dataLines, "\n")

			// Skip [DONE] sentinel from OpenAI Chat Completions
			if rawData == "[DONE]" && s.currentEvent == "" {
				s.currentEvent = ""
				s.dataLines = nil
				return
			}

			var data interface{}
			if err := json.Unmarshal([]byte(rawData), &data); err != nil {
				data = rawData
			}

			eventType := s.currentEvent
			if eventType == "" {
				eventType = "message"
			}
			s.AddEvent(eventType, data)
			s.currentEvent = ""
			s.dataLines = nil
		}
	}
}

// AddEvent appends a parsed stream event and updates the snapshot.
func (s *SSEReassembler) AddEvent(eventType string, data interface{}) {
	s.Events = append(s.Events, map[string]interface{}{
		"event": eventType,
		"data":  data,
	})
	s.accumulate(eventType, data)
}

func (s *SSEReassembler) accumulate(eventType string, data interface{}) {
	dataMap, ok := data.(map[string]interface{})
	if !ok {
		return
	}

	switch eventType {
	case "message_start":
		if msg, ok := dataMap["message"].(map[string]interface{}); ok {
			s.snapshot = deepCopyMap(msg)
		}
	case "response.created", "response.completed", "response.done":
		if response, ok := dataMap["response"].(map[string]interface{}); ok {
			s.snapshot = deepCopyMap(response)
		} else if eventType == "response.completed" || eventType == "response.done" {
			s.snapshot = deepCopyMap(dataMap)
		}
	case "message":
		if _, ok := dataMap["choices"]; ok {
			s.accumulateChatCompletionChunk(dataMap)
			return
		}
		// fall through to content block handling below
		if s.snapshot == nil {
			return
		}
	case "content_block_start":
		if s.snapshot == nil {
			return
		}
		block := deepCopyMap(getMap(dataMap, "content_block"))
		if block == nil {
			block = make(map[string]interface{})
		}
		content, _ := s.snapshot["content"].([]interface{})
		idx := getInt(dataMap, "index")
		if idx < 0 {
			idx = len(content)
		}
		// Extend content list if needed
		for len(content) <= idx {
			content = append(content, make(map[string]interface{}))
		}
		content[idx] = block
		s.snapshot["content"] = content
		return
	case "content_block_delta":
		if s.snapshot == nil {
			return
		}
		idx := getInt(dataMap, "index")
		delta := getMap(dataMap, "delta")
		if delta == nil {
			return
		}
		content, _ := s.snapshot["content"].([]interface{})
		if idx < len(content) {
			if block, ok := content[idx].(map[string]interface{}); ok {
				switch {
				case delta["type"] == "text_delta":
					block["text"] = getString(block, "text") + getString(delta, "text")
				case delta["type"] == "thinking_delta":
					block["thinking"] = getString(block, "thinking") + getString(delta, "thinking")
				case delta["type"] == "input_json_delta":
					block["_partial_json"] = getString(block, "_partial_json") + getString(delta, "partial_json")
				}
			}
		}
		return
	case "content_block_stop":
		if s.snapshot == nil {
			return
		}
		idx := getInt(dataMap, "index")
		content, _ := s.snapshot["content"].([]interface{})
		if idx < len(content) {
			if block, ok := content[idx].(map[string]interface{}); ok {
				if partialJSON, ok := block["_partial_json"].(string); ok {
					var input interface{}
					if json.Unmarshal([]byte(partialJSON), &input) == nil {
						block["input"] = input
					}
					delete(block, "_partial_json")
				}
			}
		}
		return
	case "message_delta":
		if s.snapshot == nil {
			return
		}
		if delta, ok := dataMap["delta"].(map[string]interface{}); ok {
			for k, v := range delta {
				s.snapshot[k] = v
			}
		}
		if usage, ok := dataMap["usage"].(map[string]interface{}); ok {
			snapUsage, _ := s.snapshot["usage"].(map[string]interface{})
			if snapUsage == nil {
				snapUsage = make(map[string]interface{})
				s.snapshot["usage"] = snapUsage
			}
			for k, v := range usage {
				snapUsage[k] = v
			}
		}
		return
	}

	// Default accumulation for non-standard events
	if s.snapshot == nil {
		return
	}
}

func (s *SSEReassembler) accumulateChatCompletionChunk(data map[string]interface{}) {
	choices, _ := data["choices"].([]interface{})
	usage, _ := data["usage"].(map[string]interface{})

	if len(choices) == 0 {
		if usage != nil && s.snapshot != nil {
			s.mergeChatCompletionUsage(usage)
		}
		return
	}

	choice, _ := choices[0].(map[string]interface{})
	if choice == nil {
		return
	}
	delta, _ := choice["delta"].(map[string]interface{})
	finishReason, _ := choice["finish_reason"].(string)

	if s.snapshot == nil {
		s.snapshot = map[string]interface{}{
			"id":      getString(data, "id"),
			"object":  "chat.completion",
			"model":   getString(data, "model"),
			"choices": []interface{}{map[string]interface{}{
				"index": 0,
				"message": map[string]interface{}{
					"role":    "assistant",
					"content": "",
				},
				"finish_reason": nil,
			}},
			"content": []interface{}{map[string]interface{}{"type": "text", "text": ""}},
		}
	}

	msg, _ := s.snapshot["choices"].([]interface{})
	if len(msg) > 0 {
		msg0, _ := msg[0].(map[string]interface{})
		if delta != nil {
			if role, ok := delta["role"].(string); ok && role != "" {
				msg0["role"] = role
			}
			if reasoning, ok := delta["reasoning_content"].(string); ok && reasoning != "" {
				existing := getString(msg0, "reasoning_content")
				updated := existing + reasoning
				msg0["reasoning_content"] = updated
				s.mirrorReasoningToContent(updated)
			}
			if content, ok := delta["content"].(string); ok && content != "" {
				existing := getString(msg0, "content")
				msg0["content"] = existing + content
				// Update text block
				contentBlocks, _ := s.snapshot["content"].([]interface{})
				for _, cb := range contentBlocks {
					if cbMap, ok := cb.(map[string]interface{}); ok && cbMap["type"] == "text" {
						cbMap["text"] = getString(cbMap, "text") + content
					}
				}
			}

			if toolCalls, ok := delta["tool_calls"].([]interface{}); ok {
				for _, tc := range toolCalls {
					tcMap, _ := tc.(map[string]interface{})
					if tcMap != nil {
						idx := getInt(tcMap, "index")
						existing := getMapSlice(msg0, "tool_calls")
						for len(existing) <= idx {
							existing = append(existing, map[string]interface{}{
								"id": "", "type": "function",
								"function": map[string]interface{}{"name": "", "arguments": ""},
							})
						}
						ec := existing[idx].(map[string]interface{})
						if id, ok := tcMap["id"].(string); ok {
							ec["id"] = id
						}
						if t, ok := tcMap["type"].(string); ok {
							ec["type"] = t
						}
						if fn, ok := tcMap["function"].(map[string]interface{}); ok {
							fnMap, _ := ec["function"].(map[string]interface{})
							if name, ok := fn["name"].(string); ok {
								fnMap["name"] = fnMap["name"].(string) + name
							}
							if args, ok := fn["arguments"].(string); ok {
								fnMap["arguments"] = fnMap["arguments"].(string) + args
							}
						}
						msg0["tool_calls"] = existing
						s.mirrorToolCallToContent(idx, ec)
					}
				}
			}
		}
		if finishReason != "" {
			msg0["finish_reason"] = finishReason
		}
	}
	if usage != nil {
		s.mergeChatCompletionUsage(usage)
	}
}

func (s *SSEReassembler) mirrorToolCallToContent(idx int, tc map[string]interface{}) {
	content, _ := s.snapshot["content"].([]interface{})
	offset := 1
	if s.chatCompletionThinkingBlock(false) != nil {
		offset++
	}
	target := idx + offset
	for len(content) <= target {
		content = append(content, map[string]interface{}{
			"type": "tool_use", "id": "", "name": "", "input": map[string]interface{}{},
		})
	}
	block := content[target].(map[string]interface{})
	if id, ok := tc["id"].(string); ok && id != "" {
		block["id"] = id
	}
	if fn, ok := tc["function"].(map[string]interface{}); ok {
		if name, ok := fn["name"].(string); ok && name != "" {
			block["name"] = name
		}
		if argsStr, ok := fn["arguments"].(string); ok && argsStr != "" {
			var input interface{}
			if json.Unmarshal([]byte(argsStr), &input) == nil {
				block["input"] = input
			}
		}
	}
	s.snapshot["content"] = content
}

func (s *SSEReassembler) mirrorReasoningToContent(reasoning string) {
	block := s.chatCompletionThinkingBlock(true)
	if block != nil {
		block["thinking"] = reasoning
	}
}

func (s *SSEReassembler) chatCompletionThinkingBlock(create bool) map[string]interface{} {
	content, _ := s.snapshot["content"].([]interface{})
	for _, c := range content {
		if cm, ok := c.(map[string]interface{}); ok && cm["type"] == "thinking" {
			return cm
		}
	}
	if !create {
		return nil
	}
	block := map[string]interface{}{"type": "thinking", "thinking": ""}
	content = append([]interface{}{block}, content...)
	s.snapshot["content"] = content
	return block
}

func (s *SSEReassembler) mergeChatCompletionUsage(usage map[string]interface{}) {
	existing, _ := s.snapshot["usage"].(map[string]interface{})
	if existing == nil {
		existing = make(map[string]interface{})
		s.snapshot["usage"] = existing
	}
	for k, v := range usage {
		existing[k] = v
	}
}

// Reconstruct returns the accumulated snapshot.
func (s *SSEReassembler) Reconstruct() map[string]interface{} {
	return s.snapshot
}

// --- helpers ---

func indexByte(data []byte, b byte) int {
	for i, c := range data {
		if c == b {
			return i
		}
	}
	return -1
}

func deepCopyMap(src map[string]interface{}) map[string]interface{} {
	data, _ := json.Marshal(src)
	var dst map[string]interface{}
	json.Unmarshal(data, &dst)
	return dst
}

func getMap(m map[string]interface{}, key string) map[string]interface{} {
	if v, ok := m[key].(map[string]interface{}); ok {
		return v
	}
	return nil
}

func getString(m map[string]interface{}, key string) string {
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}

func getInt(m map[string]interface{}, key string) int {
	switch v := m[key].(type) {
	case float64:
		return int(v)
	case int:
		return v
	case json.Number:
		n, _ := v.Int64()
		return int(n)
	}
	return -1
}

func getMapSlice(m map[string]interface{}, key string) []interface{} {
	v, _ := m[key].([]interface{})
	return v
}
