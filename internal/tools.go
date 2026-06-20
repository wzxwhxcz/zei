package internal

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"sync/atomic"
	"time"

	"zai-proxy/internal/toolcall"
)

// 工具调用的解析与提示词生成委托给 internal/toolcall 包（移植自 CJackHwang/ds2api，
// 并把 DSML 品牌化为 GLMML：<|GLMML|tool_calls>...）。本文件只保留：
//   - OpenAI 格式的 Tool / ToolCall 类型
//   - z.ai 特有的角色消息文本化（assistant 工具调用 → 文本、tool 结果 → user 消息）
//   - 三层接入：GenerateToolPrompt / ExtractToolInvocationsWithFallback / RemoveToolJSONContent

type Tool struct {
	Type     string       `json:"type"`
	Function ToolFunction `json:"function,omitempty"`
}

type ToolFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}
type ToolCall struct {
	Index    int              `json:"index,omitempty"`
	ID       string           `json:"id"`
	Type     string           `json:"type"`
	Function ToolCallFunction `json:"function"`
}
type ToolCallFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

var (
	callIDCounter int64
	// RemoveToolJSONContent 用：剥离整段工具调用标记块。同时兼容
	//   - GLMML（首选）：<|GLMML|tool_calls>...</|GLMML|tool_calls>
	//   - legacy XML（兜底）：<tool_calls>...</tool_calls>
	// dotall 让 . 跨行；非贪婪避免误吞多段。
	glmmlToolCallBlockPattern = regexp.MustCompile(`(?s)<\|GLMML\|tool_calls>.*?</\|GLMML\|tool_calls>`)
	xmlToolCallBlockPattern   = regexp.MustCompile(`(?is)<tool_calls>.*?</tool_calls>`)
)

// GenerateToolPrompt 构造工具调用提示词，注入 system / 第一条 user 消息。
// 用 toolcall.BuildToolCallInstructions 生成 GLMML 格式的统一指令块（含正负示例）。
func GenerateToolPrompt(tools []Tool, toolChoice interface{}) string {
	if len(tools) == 0 {
		return ""
	}
	toolNames := make([]string, 0, len(tools))
	for _, t := range tools {
		if t.Type == "function" {
			toolNames = append(toolNames, t.Function.Name)
		}
	}
	if len(toolNames) == 0 {
		return ""
	}
	instructions := toolcall.BuildToolCallInstructions(toolNames)

	// tool_choice 处理：auto（默认）/ required / 指定函数。
	switch tc := toolChoice.(type) {
	case string:
		if tc == "required" {
			instructions += "\n\n本次回复你必须至少调用一个上面列出的工具。"
		}
	case map[string]interface{}:
		if tc["type"] == "function" {
			if fn, ok := tc["function"].(map[string]interface{}); ok {
				if name, ok := fn["name"].(string); ok {
					instructions += "\n\n本次回复你必须只调用工具：" + name + "，不要调用其它工具。"
				}
			}
		}
	}
	return instructions
}

// ProcessMessagesWithTools 把 OpenAI 格式的工具消息文本化（z.ai 不认 tool_calls/tool 角色），
// 并把工具调用提示词注入 system 与第一条 user 消息。z.ai 会覆盖客户端 system 消息，
// 所以必须同时注入到 user 消息里。
func ProcessMessagesWithTools(messages []Message, tools []Tool, toolChoice interface{}) []Message {
	if !Cfg.ToolSupport || len(tools) == 0 {
		LogDebug("[Tools] Tool support disabled or no tools provided")
		return messages
	}
	if tc, ok := toolChoice.(string); ok && tc == "none" {
		LogDebug("[Tools] Tool choice is 'none', skipping tool processing")
		return messages
	}

	toolPrompt := GenerateToolPrompt(tools, toolChoice)
	if toolPrompt == "" {
		LogDebug("[Tools] Generated empty tool prompt")
		return messages
	}
	LogDebug("[Tools] Injecting GLMML tool prompt for %d tools", len(tools))

	processed := make([]Message, len(messages))
	copy(processed, messages)
	for i, msg := range processed {
		if msg.Role == "assistant" && len(msg.ToolCalls) > 0 {
			processed[i] = convertAssistantToolCallMessage(msg)
		} else if msg.Role == "tool" {
			processed[i] = convertToolMessage(msg)
		}
	}

	// 1) 追加到已有 system 消息
	injected := false
	for i, msg := range processed {
		if msg.Role == "system" {
			content, _ := msg.ParseContent()
			processed[i].Content = strings.TrimSpace(content) + "\n\n" + toolPrompt
			injected = true
			break
		}
	}
	// 2) 同时前置注入到第一条 user 消息（z.ai 会覆盖客户端 system）
	for i, msg := range processed {
		if msg.Role == "user" {
			content, _ := msg.ParseContent()
			processed[i].Content = toolPrompt + "\n\n---\n\n" + content
			break
		}
	}
	_ = injected
	return processed
}

func convertAssistantToolCallMessage(msg Message) Message {
	content, _ := msg.ParseContent()
	var sb strings.Builder
	if content != "" {
		sb.WriteString(content)
		sb.WriteString("\n\n")
	}
	sb.WriteString("[已执行工具调用]\n")
	for _, tc := range msg.ToolCalls {
		sb.WriteString(fmt.Sprintf("- 调用了 %s，参数: %s (call_id: %s)\n", tc.Function.Name, tc.Function.Arguments, tc.ID))
	}
	return Message{
		Role:    "assistant",
		Content: sb.String(),
	}
}

func convertToolMessage(msg Message) Message {
	content, _ := msg.ParseContent()
	var resultText string
	if msg.ToolCallID != "" {
		resultText = fmt.Sprintf("[工具返回结果] (call_id: %s)\n以下是工具返回的数据，请直接使用这些数据回答用户：\n%s", msg.ToolCallID, content)
	} else {
		resultText = fmt.Sprintf("[工具返回结果]\n以下是工具返回的数据，请直接使用这些数据回答用户：\n%s", content)
	}
	return Message{
		Role:    "user",
		Content: resultText,
	}
}

// ExtractToolInvocations 从模型输出文本里解析工具调用（GLMML 首选，兼容 legacy XML）。
func ExtractToolInvocations(text string) []ToolCall {
	return ExtractToolInvocationsWithFallback(text, "")
}

// ExtractToolInvocationsWithFallback 先从 content 提取，找不到则回退到 reasoning（思考链）。
// GLM 系列常把工具调用写在思考链里。availableToolNames 传 nil 表示不按名单过滤。
func ExtractToolInvocationsWithFallback(content, reasoning string) []ToolCall {
	// 1) 先解析 content。
	parsed := toolcall.ParseStandaloneToolCallsDetailed(content, nil)
	if len(parsed.Calls) == 0 {
		// 2) content 没解析到 → 回退到 reasoning（z.ai/GLM 常把工具调用写在思考链里，
		//    而且此时 content 往往非空是自然语言，所以这里无视 content 是否为空都回退）。
		if strings.TrimSpace(reasoning) != "" {
			parsed = toolcall.ParseStandaloneToolCallsDetailed(reasoning, nil)
			if len(parsed.Calls) > 0 {
				LogDebug("[Tools] Extracted %d tool calls from reasoning fallback", len(parsed.Calls))
			}
		}
	}
	if len(parsed.Calls) == 0 {
		if parsed.SawToolCallSyntax {
			LogDebug("[Tools] Saw tool-call syntax but parsed 0 calls (likely truncated/malformed)")
		}
		return nil
	}
	calls := make([]ToolCall, 0, len(parsed.Calls))
	for _, c := range parsed.Calls {
		args, _ := json.Marshal(c.Input)
		calls = append(calls, ToolCall{
			ID:   generateCallID(),
			Type: "function",
			Function: ToolCallFunction{
				Name:      c.Name,
				Arguments: string(args),
			},
		})
	}
	LogDebug("[Tools] Extracted %d tool calls via GLMML parser", len(calls))
	return calls
}

// RemoveToolJSONContent 从文本里剥离整段工具调用标记，返回干净的自然语言内容。
// 兼容 GLMML（<|GLMML|tool_calls>...）和 legacy XML（<tool_calls>...）。
func RemoveToolJSONContent(text string) string {
	if text == "" {
		return text
	}
	cleaned := glmmlToolCallBlockPattern.ReplaceAllString(text, "")
	cleaned = xmlToolCallBlockPattern.ReplaceAllString(cleaned, "")
	return strings.TrimSpace(cleaned)
}

func generateCallID() string {
	seq := atomic.AddInt64(&callIDCounter, 1)
	return fmt.Sprintf("call_%d_%d", time.Now().UnixMilli(), seq)
}
