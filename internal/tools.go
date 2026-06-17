package internal

import (
	"bytes"
	"encoding/json"
	"fmt"
	"html"
	"regexp"
	"strings"
	"sync/atomic"
	"time"
)

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
	toolCallFencePattern       = regexp.MustCompile("(?s)```(?:json|xml)?\\s*(.*?)\\s*```")
	functionCallPattern        = regexp.MustCompile(`(?s)调用函数\s*[：:]\s*([\w\-\.]+)\s*(?:参数|arguments)[：:]\s*(\{.*?\})`)
	functionInvokePattern      = regexp.MustCompile(`(?s)\b([\w\-\.]+)\s*\(\s*(\{.*?\})\s*\)`)
	xmlToolCallBlockPattern    = regexp.MustCompile(`(?is)<tool_calls>\s*(.*?)\s*</tool_calls>`)
	xmlToolCallItemPattern     = regexp.MustCompile(`(?is)<tool_call(?:\s+id="([^"]+)")?>(.*?)</tool_call>`)
	xmlToolNamePattern         = regexp.MustCompile(`(?is)<name>\s*([^<]+?)\s*</name>`)
	xmlToolArgumentsPattern    = regexp.MustCompile(`(?is)<arguments>\s*(.*?)\s*</arguments>`)
	// GLM-5.2 自创格式：每个参数用 <parameter name="x">value</parameter> 表示
	glmToolParamPattern        = regexp.MustCompile(`(?is)<parameter(?:\s+name="([^"]+)")?>(.*?)</parameter>`)
	glmToolParametersPattern   = regexp.MustCompile(`(?is)<parameters>\s*(.*?)\s*</parameters>`)
	xmlToolArgumentsCDataStart = "<![CDATA["
	xmlToolArgumentsCDataEnd   = "]]>"
	callIDCounter              int64
)

func GenerateToolPrompt(tools []Tool, toolChoice interface{}) string {
	if len(tools) == 0 {
		return ""
	}

	var toolDefs []string
	var toolNames []string
	for _, tool := range tools {
		if tool.Type != "function" {
			continue
		}

		fn := tool.Function
		toolNames = append(toolNames, fn.Name)
		toolInfo := fmt.Sprintf("<tool>\n<name>%s</name>", html.EscapeString(fn.Name))
		if fn.Description != "" {
			toolInfo += fmt.Sprintf("\n<description>%s</description>", html.EscapeString(fn.Description))
		}

		if len(fn.Parameters) > 0 {
			var prettyParams bytes.Buffer
			if err := json.Indent(&prettyParams, fn.Parameters, "", "  "); err == nil {
				toolInfo += fmt.Sprintf("\n<parameters><![CDATA[%s]]></parameters>", prettyParams.String())
			} else {
				toolInfo += fmt.Sprintf("\n<parameters><![CDATA[%s]]></parameters>", string(fn.Parameters))
			}
		}

		toolInfo += "\n</tool>"
		toolDefs = append(toolDefs, toolInfo)
	}

	if len(toolDefs) == 0 {
		return ""
	}

	instructions := getToolChoiceInstructions(toolChoice, toolNames)
	examples := buildToolExamples(toolNames)

	return "<tool_injection>\n<available_tools>\n" + strings.Join(toolDefs, "\n") + "\n</available_tools>\n" + instructions + examples + "\n</tool_injection>"
}

// buildToolExamples 用真实工具名生成强示例。
// 灵感来自 CJackHwang/ds2api：用真实工具名 + 多个正负示例显著提升 GLM 类模型的工具调用率。
func buildToolExamples(toolNames []string) string {
	if len(toolNames) == 0 {
		return ""
	}
	primary := toolNames[0]

	// 尝试列出第一个工具的"single call"示例
	example := fmt.Sprintf(`
<correct_examples>
<!-- 示例 1：调用单个工具 -->
<tool_calls>
  <tool_call id="call_1">
    <name>%s</name>
    <arguments><![CDATA[{}]]></arguments>
  </tool_call>
</tool_calls>
</correct_examples>

<wrong_examples>
<!-- 错误 1：用文字假装调用，绝对禁止 -->
我已经为您调用了 %s 工具，结果是 ...

<!-- 错误 2：解释自己不能调用，绝对禁止 -->
作为 AI 我无法直接执行工具，建议您...

<!-- 错误 3：把 XML 包在 markdown 代码块里 -->
` + "```" + `xml
<tool_calls>...</tool_calls>
` + "```" + `

<!-- 错误 4：在 XML 后追加解释文字 -->
<tool_calls>...</tool_calls>
希望对您有帮助！
</wrong_examples>

<final_directive>
你是一个能调用工具的智能体。这些工具是真实的、可执行的，会被外部系统接收并执行。
当用户请求需要用到工具时，**直接输出 &lt;tool_calls&gt; XML 块，不要解释、不要道歉、不要假装无法调用**。
工具会真的被调用，调用结果会在下一轮以 [工具返回结果] 标记返回给你。
</final_directive>`, primary, primary)

	return example
}

func getToolChoiceInstructions(toolChoice interface{}, toolNames []string) string {
	allowedTools := html.EscapeString(strings.Join(toolNames, ", "))
	baseInstructions := fmt.Sprintf(`<call_protocol>
<allowed_tools>%s</allowed_tools>
<response_format>
  <tool_calls>
    <tool_call id="call_1">
      <name>函数名</name>
      <arguments><![CDATA[{"参数名":"参数值"}]]></arguments>
    </tool_call>
  </tool_calls>
</response_format>
<rules>
  <rule index="1">只能调用上面列出的函数名，不能改名，不能替换成别的工具。</rule>
  <rule index="2">当需要调用工具时，只输出 XML 工具调用，不要附带解释、Markdown、代码块或额外文本。</rule>
  <rule index="3">arguments 必须是合法 JSON；如果没有参数，使用 {}。</rule>
  <rule index="4">如果用户要求使用工具或 tool_choice 有要求，你必须先调用工具，不能先解释为什么不能调用。</rule>
  <rule index="5">即使信息不完整，也要先依据已有上下文构造最合理的参数发起调用。</rule>
  <rule index="6">如果已经收到工具结果，必须直接根据结果回答，不能重复调用工具。</rule>
  <rule index="7">不要把 &lt;tool_calls&gt;、&lt;tool_call&gt;、&lt;name&gt;、&lt;arguments&gt; 当成普通回答内容输出给用户。</rule>
</rules>
<tool_result_handling>
  <assistant_marker>[已执行工具调用]</assistant_marker>
  <user_marker>[工具返回结果]</user_marker>
  <instruction>当你看到这些标记时，说明工具已经被调用并返回了结果。你必须直接使用工具返回的数据回答用户，绝对不要再次调用工具。</instruction>
</tool_result_handling>
</call_protocol>`, allowedTools)

	switch tc := toolChoice.(type) {
	case string:
		if tc == "required" {
			return baseInstructions + "\n<tool_choice mode=\"required\" />"
		}
		return baseInstructions + "\n<tool_choice mode=\"auto\" />"
	case map[string]interface{}:
		if tc["type"] == "function" {
			if fn, ok := tc["function"].(map[string]interface{}); ok {
				if name, ok := fn["name"].(string); ok {
					return baseInstructions + fmt.Sprintf("\n<tool_choice mode=\"required\" tool=\"%s\" />", html.EscapeString(name))
				}
			}
		}
	}

	return baseInstructions + "\n<tool_choice mode=\"auto\" />"
}

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
	LogDebug("[Tools] Injecting tool prompt for %d tools", len(tools))

	processed := make([]Message, len(messages))
	copy(processed, messages)
	for i, msg := range processed {
		if msg.Role == "assistant" && len(msg.ToolCalls) > 0 {
			processed[i] = convertAssistantToolCallMessage(msg)
		} else if msg.Role == "tool" {
			processed[i] = convertToolMessage(msg)
		}
	}

	// 注入策略：同时把 toolPrompt 插进 system 消息和第一条 user 消息。
	// 关键发现：z.ai 上游会丢弃/覆盖客户端传入的 system 消息（用平台自己的 "你是 GLM 助手" 覆盖）。
	// 实测对 GLM-5.1 用 DIAGNOSTIC_KEYWORD 测试 system 消息，模型反馈"我没看到这个关键字"。
	// 因此必须把工具说明追加到 user 消息，模型才能真正"看到"工具。
	hasSystem := false
	for i, msg := range processed {
		if msg.Role == "system" {
			hasSystem = true
			processed[i].Content = appendTextToContent(msg.Content, toolPrompt)
			break
		}
	}
	if !hasSystem {
		systemMsg := Message{
			Role:    "system",
			Content: "<system_instructions>\n<assistant_identity>你是一个能调用工具的智能助手。</assistant_identity>\n" + toolPrompt + "\n</system_instructions>",
		}
		processed = append([]Message{systemMsg}, processed...)
	}

	// 关键改进：把工具说明也注入到第一条 user 消息前面（user 消息 z.ai 不会覆盖）
	for i, msg := range processed {
		if msg.Role == "user" {
			userToolBrief := "[可用工具说明 — 重要]\n" + toolPrompt + "\n\n[用户原始问题]\n"
			processed[i].Content = prependTextToContent(msg.Content, userToolBrief)
			break
		}
	}

	return processed
}

// prependTextToContent 把 prefix 加到 content 前面（兼容字符串和复杂结构）
func prependTextToContent(content interface{}, prefix string) interface{} {
	switch v := content.(type) {
	case string:
		return prefix + v
	case nil:
		return prefix
	default:
		return prefix + fmt.Sprintf("%v", v)
	}
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

func appendTextToContent(content interface{}, suffix string) interface{} {
	switch c := content.(type) {
	case string:
		return c + suffix
	case []interface{}:
		result := make([]interface{}, len(c))
		copy(result, c)
		lastTextIdx := -1
		for i, item := range result {
			if part, ok := item.(map[string]interface{}); ok {
				if partType, _ := part["type"].(string); partType == "text" {
					lastTextIdx = i
				}
			}
		}

		if lastTextIdx >= 0 {
			if part, ok := result[lastTextIdx].(map[string]interface{}); ok {
				newPart := make(map[string]interface{})
				for k, v := range part {
					newPart[k] = v
				}
				if text, ok := newPart["text"].(string); ok {
					newPart["text"] = text + suffix
				}
				result[lastTextIdx] = newPart
			}
		} else {
			result = append(result, map[string]interface{}{
				"type": "text",
				"text": suffix,
			})
		}
		return result
	default:
		return content
	}
}
func findMatchingBrace(text string, start int) int {
	if start >= len(text) || text[start] != '{' {
		return -1
	}
	braceCount := 1
	inString := false
	escapeNext := false
	j := start + 1
	for j < len(text) && braceCount > 0 {
		ch := text[j]
		if escapeNext {
			escapeNext = false
			j++
			continue
		}
		switch ch {
		case '\\':
			if inString {
				escapeNext = true
			}
		case '"':
			inString = !inString
		case '{':
			if !inString {
				braceCount++
			}
		case '}':
			if !inString {
				braceCount--
			}
		}
		j++
	}
	if braceCount != 0 {
		return -1
	}
	return j
}

func normalizeArguments(args interface{}) string {
	switch v := args.(type) {
	case string:
		v = strings.TrimSpace(v)
		if v == "" {
			return "{}"
		}
		var check json.RawMessage
		if json.Unmarshal([]byte(v), &check) == nil {
			return v
		}
		fixed := strings.ReplaceAll(v, "'", "\"")
		if json.Unmarshal([]byte(fixed), &check) == nil {
			return fixed
		}
		return v
	case map[string]interface{}:
		if b, err := json.Marshal(v); err == nil {
			return string(b)
		}
	case []interface{}:
		if b, err := json.Marshal(v); err == nil {
			return string(b)
		}
	case nil:
		return "{}"
	}
	return "{}"
}

func validateAndNormalizeCalls(calls []ToolCall) []ToolCall {
	if len(calls) == 0 {
		return nil
	}
	var valid []ToolCall
	for _, call := range calls {
		if call.Function.Name == "" {
			LogDebug("[Tools] Skipping tool call with empty function name")
			continue
		}
		if call.ID == "" {
			call.ID = generateCallID()
		}
		if call.Type == "" {
			call.Type = "function"
		}
		call.Function.Arguments = normalizeArguments(call.Function.Arguments)
		valid = append(valid, call)
	}
	if len(valid) == 0 {
		return nil
	}
	return valid
}

func parseNamedFunctionObject(jsonStr string) []ToolCall {
	var raw struct {
		ID        string      `json:"id"`
		Type      string      `json:"type"`
		Name      string      `json:"name"`
		Arguments interface{} `json:"arguments"`
		Tool      string      `json:"tool"`
		Args      interface{} `json:"args"`
		Input     interface{} `json:"input"`
		Function  *struct {
			Name      string      `json:"name"`
			Arguments interface{} `json:"arguments"`
		} `json:"function"`
	}
	if err := json.Unmarshal([]byte(jsonStr), &raw); err != nil {
		return nil
	}

	name := raw.Name
	args := raw.Arguments
	if raw.Function != nil {
		if name == "" {
			name = raw.Function.Name
		}
		if args == nil {
			args = raw.Function.Arguments
		}
	}
	if name == "" && raw.Tool != "" {
		name = raw.Tool
	}
	if args == nil && raw.Args != nil {
		args = raw.Args
	}
	if args == nil && raw.Input != nil {
		args = raw.Input
	}
	if name == "" {
		return nil
	}
	return []ToolCall{{
		ID:   raw.ID,
		Type: raw.Type,
		Function: ToolCallFunction{
			Name:      name,
			Arguments: normalizeArguments(args),
		},
	}}
}

func trimXMLArgumentsPayload(payload string) string {
	payload = strings.TrimSpace(payload)
	if strings.HasPrefix(payload, xmlToolArgumentsCDataStart) && strings.HasSuffix(payload, xmlToolArgumentsCDataEnd) {
		payload = strings.TrimPrefix(payload, xmlToolArgumentsCDataStart)
		payload = strings.TrimSuffix(payload, xmlToolArgumentsCDataEnd)
	}
	return strings.TrimSpace(html.UnescapeString(payload))
}

// extractGLMStyleArguments 解析 GLM-5.2 自创的 <parameters><parameter name="x">val</parameter></parameters> 格式，
// 把每个 <parameter name="x">value</parameter> 收集成 {"x":"value"} JSON。
// 返回空串表示没有 <parameters> 块；返回 "{}" 表示空参数。
func extractGLMStyleArguments(itemPayload string) string {
	paramsBlock := glmToolParametersPattern.FindStringSubmatch(itemPayload)
	if len(paramsBlock) < 2 {
		return ""
	}
	paramMatches := glmToolParamPattern.FindAllStringSubmatch(paramsBlock[1], -1)
	if len(paramMatches) == 0 {
		return "{}"
	}
	obj := make(map[string]string, len(paramMatches))
	for _, pm := range paramMatches {
		if len(pm) < 3 {
			continue
		}
		name := strings.TrimSpace(pm[1])
		val := strings.TrimSpace(html.UnescapeString(pm[2]))
		if name == "" {
			continue
		}
		obj[name] = val
	}
	b, err := json.Marshal(obj)
	if err != nil {
		return "{}"
	}
	return string(b)
}

// extractItemArguments 从一个 <tool_call> 的内部 payload 取 arguments：
// 优先标准 <arguments><![CDATA[...]]></arguments>，没有就回退到 GLM-5.2 的 <parameters><parameter>。
func extractItemArguments(itemPayload string) string {
	if argsMatch := xmlToolArgumentsPattern.FindStringSubmatch(itemPayload); len(argsMatch) >= 2 {
		args := trimXMLArgumentsPayload(argsMatch[1])
		if args != "" {
			return args
		}
	}
	if glmArgs := extractGLMStyleArguments(itemPayload); glmArgs != "" {
		return glmArgs
	}
	return "{}"
}

func parseXMLToolCalls(text string) []ToolCall {
	blocks := xmlToolCallBlockPattern.FindAllStringSubmatch(text, -1)
	for _, block := range blocks {
		if len(block) < 2 {
			continue
		}
		itemMatches := xmlToolCallItemPattern.FindAllStringSubmatch(block[1], -1)
		if len(itemMatches) == 0 {
			continue
		}

		calls := make([]ToolCall, 0, len(itemMatches))
		for _, item := range itemMatches {
			if len(item) < 3 {
				continue
			}
			nameMatch := xmlToolNamePattern.FindStringSubmatch(item[2])
			if len(nameMatch) < 2 {
				continue
			}
			calls = append(calls, ToolCall{
				ID:   strings.TrimSpace(item[1]),
				Type: "function",
				Function: ToolCallFunction{
					Name:      strings.TrimSpace(html.UnescapeString(nameMatch[1])),
					Arguments: extractItemArguments(item[2]),
				},
			})
		}
		if len(calls) > 0 {
			return calls
		}
	}

	itemMatches := xmlToolCallItemPattern.FindAllStringSubmatch(text, -1)
	if len(itemMatches) == 0 {
		return nil
	}
	calls := make([]ToolCall, 0, len(itemMatches))
	for _, item := range itemMatches {
		if len(item) < 3 {
			continue
		}
		payload := strings.TrimSpace(item[2])
		if callsFromJSON := parseToolCallsJSON(payload); callsFromJSON != nil {
			return callsFromJSON
		}
		if callsFromJSON := parseNamedFunctionObject(payload); callsFromJSON != nil {
			return callsFromJSON
		}
		nameMatch := xmlToolNamePattern.FindStringSubmatch(payload)
		if len(nameMatch) < 2 {
			continue
		}
		calls = append(calls, ToolCall{
			ID:   strings.TrimSpace(item[1]),
			Type: "function",
			Function: ToolCallFunction{
				Name:      strings.TrimSpace(html.UnescapeString(nameMatch[1])),
				Arguments: extractItemArguments(payload),
			},
		})
	}
	if len(calls) == 0 {
		return nil
	}
	return calls
}

func parseTaggedToolPayload(text string) []ToolCall {
	if calls := parseXMLToolCalls(text); calls != nil {
		return calls
	}
	return nil
}

func parseFunctionInvocation(text string) []ToolCall {
	matches := functionInvokePattern.FindAllStringSubmatch(text, -1)
	for _, match := range matches {
		if len(match) < 3 {
			continue
		}
		name := strings.TrimSpace(match[1])
		args := strings.TrimSpace(match[2])
		var check json.RawMessage
		if name != "" && json.Unmarshal([]byte(args), &check) == nil {
			return []ToolCall{{
				Type: "function",
				Function: ToolCallFunction{
					Name:      name,
					Arguments: args,
				},
			}}
		}
	}
	return nil
}

// ExtractToolInvocations 从响应文本中提取工具调用
func ExtractToolInvocations(text string) []ToolCall {
	if text == "" {
		return nil
	}

	scanText := text
	if Cfg != nil && len(scanText) > Cfg.ScanLimit {
		scanText = scanText[:Cfg.ScanLimit]
	}

	if calls := parseTaggedToolPayload(scanText); calls != nil {
		LogDebug("[ExtractToolInvocations] Found XML tool payload")
		return validateAndNormalizeCalls(calls)
	}

	matches := toolCallFencePattern.FindAllStringSubmatch(scanText, -1)
	for _, match := range matches {
		if len(match) <= 1 {
			continue
		}
		payload := strings.TrimSpace(match[1])
		if calls := parseTaggedToolPayload(payload); calls != nil {
			LogDebug("[ExtractToolInvocations] Found XML tool payload in fence")
			return validateAndNormalizeCalls(calls)
		}
		if calls := parseToolCallsJSON(payload); calls != nil {
			LogDebug("[ExtractToolInvocations] Found %d tool calls in JSON fence", len(calls))
			return validateAndNormalizeCalls(calls)
		}
		if calls := parseNamedFunctionObject(payload); calls != nil {
			LogDebug("[ExtractToolInvocations] Found named function object in fence")
			return validateAndNormalizeCalls(calls)
		}
	}

	if calls := extractInlineToolCalls(scanText); calls != nil {
		LogDebug("[ExtractToolInvocations] Found %d tool calls inline", len(calls))
		return validateAndNormalizeCalls(calls)
	}

	if calls := extractSingleFunctionCall(scanText); calls != nil {
		LogDebug("[ExtractToolInvocations] Found single function call")
		return validateAndNormalizeCalls(calls)
	}

	if calls := parseFunctionInvocation(scanText); calls != nil {
		LogDebug("[ExtractToolInvocations] Found function invocation pattern")
		return validateAndNormalizeCalls(calls)
	}

	if match := functionCallPattern.FindStringSubmatch(scanText); len(match) > 2 {
		funcName := strings.TrimSpace(match[1])
		argsStr := strings.TrimSpace(match[2])
		var check json.RawMessage
		if json.Unmarshal([]byte(argsStr), &check) == nil {
			LogDebug("[ExtractToolInvocations] Found natural language function call: %s", funcName)
			return validateAndNormalizeCalls([]ToolCall{{
				Type: "function",
				Function: ToolCallFunction{
					Name:      funcName,
					Arguments: argsStr,
				},
			}})
		}
	}

	return nil
}

func extractSingleFunctionCall(text string) []ToolCall {
	searchStart := 0
	for {
		idx := strings.Index(text[searchStart:], `"name"`)
		if idx == -1 {
			break
		}
		idx += searchStart

		braceStart := -1
		for k := idx - 1; k >= 0; k-- {
			ch := text[k]
			if ch == '{' {
				braceStart = k
				break
			}
			if ch != ' ' && ch != '\t' && ch != '\n' && ch != '\r' {
				break
			}
		}
		if braceStart == -1 {
			searchStart = idx + 1
			continue
		}

		end := findMatchingBrace(text, braceStart)
		if end == -1 {
			searchStart = idx + 1
			continue
		}

		jsonStr := text[braceStart:end]
		if calls := parseNamedFunctionObject(jsonStr); calls != nil {
			return calls
		}
		searchStart = idx + 1
	}
	return nil
}
func parseToolCallsJSON(jsonStr string) []ToolCall {
	var data struct {
		ToolCalls []struct {
			ID        string      `json:"id"`
			Type      string      `json:"type"`
			Name      string      `json:"name"`
			Arguments interface{} `json:"arguments"`
			Function  interface{} `json:"function"`
		} `json:"tool_calls"`
	}
	if err := json.Unmarshal([]byte(jsonStr), &data); err != nil {
		return nil
	}
	if len(data.ToolCalls) == 0 {
		return nil
	}
	var calls []ToolCall
	for _, tc := range data.ToolCalls {
		call := ToolCall{
			ID:   tc.ID,
			Type: tc.Type,
		}
		if fn, ok := tc.Function.(map[string]interface{}); ok {
			call.Function.Name, _ = fn["name"].(string)
			if args, ok := fn["arguments"]; ok {
				call.Function.Arguments = normalizeArguments(args)
			}
		}
		if call.Function.Name == "" {
			call.Function.Name = tc.Name
		}
		if call.Function.Arguments == "" {
			if tc.Arguments != nil {
				call.Function.Arguments = normalizeArguments(tc.Arguments)
			} else {
				call.Function.Arguments = "{}"
			}
		}
		calls = append(calls, call)
	}
	return calls
}

func extractInlineToolCalls(text string) []ToolCall {
	if !strings.Contains(text, `"tool_calls"`) {
		return nil
	}
	for i := 0; i < len(text); i++ {
		if text[i] != '{' {
			continue
		}
		end := findMatchingBrace(text, i)
		if end == -1 {
			continue
		}
		jsonStr := text[i:end]
		if strings.Contains(jsonStr, `"tool_calls"`) {
			if calls := parseToolCallsJSON(jsonStr); calls != nil {
				return calls
			}
		}
		i = end - 1
	}
	return nil
}

func isToolPayload(jsonStr string) bool {
	return parseToolCallsJSON(jsonStr) != nil || parseNamedFunctionObject(jsonStr) != nil
}

func RemoveToolJSONContent(text string) string {
	result := xmlToolCallBlockPattern.ReplaceAllString(text, "")
	result = xmlToolCallItemPattern.ReplaceAllString(result, "")
	result = toolCallFencePattern.ReplaceAllStringFunc(result, func(match string) string {
		submatch := toolCallFencePattern.FindStringSubmatch(match)
		if len(submatch) > 1 {
			payload := strings.TrimSpace(submatch[1])
			if parseTaggedToolPayload(payload) != nil || isToolPayload(payload) {
				return ""
			}
		}
		return match
	})
	result = removeInlineToolCallJSON(result)
	result = removeInlineSingleFunctionCallJSON(result)
	return strings.TrimSpace(result)
}

func removeInlineSingleFunctionCallJSON(text string) string {
	for i := 0; i < len(text); i++ {
		if text[i] != '{' {
			continue
		}
		end := findMatchingBrace(text, i)
		if end == -1 {
			continue
		}
		jsonStr := text[i:end]
		if parseNamedFunctionObject(jsonStr) != nil {
			return strings.TrimSpace(text[:i] + text[end:])
		}
		i = end - 1
	}
	return text
}
func removeInlineToolCallJSON(text string) string {
	if !strings.Contains(text, `"tool_calls"`) {
		return text
	}
	var result strings.Builder
	result.Grow(len(text))
	i := 0
	for i < len(text) {
		if text[i] != '{' {
			result.WriteByte(text[i])
			i++
			continue
		}
		end := findMatchingBrace(text, i)
		if end == -1 {
			result.WriteByte(text[i])
			i++
			continue
		}
		jsonStr := text[i:end]
		if strings.Contains(jsonStr, `"tool_calls"`) {
			var data map[string]interface{}
			if err := json.Unmarshal([]byte(jsonStr), &data); err == nil {
				if _, ok := data["tool_calls"]; ok {
					i = end
					continue
				}
			}
		}
		result.WriteByte(text[i])
		i++
	}
	return result.String()
}

func generateCallID() string {
	seq := atomic.AddInt64(&callIDCounter, 1)
	return fmt.Sprintf("call_%d_%d", time.Now().UnixMilli(), seq)
}

// repairTruncatedToolCallsXML 修复截断的 <tool_calls> XML（z.ai 上游有时会在 phase 切换时截断输出）
// 如果发现未闭合的 <tool_calls> / <tool_call> / <arguments>，按嵌套层级补全闭合标签。
func repairTruncatedToolCallsXML(text string) string {
	if !strings.Contains(text, "<tool_calls>") {
		return text
	}
	// 已经闭合，无需修复
	if strings.Contains(text, "</tool_calls>") {
		return text
	}
	// 自动补全：从最内层往外补
	repaired := text
	if strings.Contains(repaired, "<arguments>") && !strings.Contains(repaired, "</arguments>") {
		// 处理 CDATA 未闭合
		if strings.Contains(repaired, "<![CDATA[") && !strings.Contains(repaired, "]]>") {
			repaired += "]]>"
		}
		repaired += "</arguments>"
	}
	if strings.Contains(repaired, "<tool_call ") || strings.Contains(repaired, "<tool_call>") {
		// 计算 <tool_call ...> 和 </tool_call> 数量差
		open := strings.Count(repaired, "<tool_call")
		close := strings.Count(repaired, "</tool_call")
		for i := close; i < open; i++ {
			repaired += "</tool_call>"
		}
	}
	if !strings.Contains(repaired, "</tool_calls>") {
		repaired += "</tool_calls>"
	}
	return repaired
}

// ExtractToolInvocationsWithFallback 主要从 content 中提取工具调用，
// 当 content 中找不到时回退到 reasoning_content（思考链）。
// GLM 系列模型经常把工具调用 XML 写在思考链里而不是回复里。
// 参考 CJackHwang/ds2api 的设计。
func ExtractToolInvocationsWithFallback(content, reasoning string) []ToolCall {
	// 先尝试修复 z.ai 截断的 XML
	repairedContent := repairTruncatedToolCallsXML(content)
	calls := ExtractToolInvocations(repairedContent)
	if len(calls) > 0 {
		if repairedContent != content {
			LogDebug("[Tools] Successfully repaired truncated XML, found %d calls", len(calls))
		}
		return calls
	}
	if strings.TrimSpace(reasoning) == "" {
		return calls
	}
	repairedReasoning := repairTruncatedToolCallsXML(reasoning)
	calls = ExtractToolInvocations(repairedReasoning)
	if len(calls) > 0 {
		LogDebug("[Tools] Extracted %d tool calls from reasoning_content (fallback)", len(calls))
	}
	return calls
}
