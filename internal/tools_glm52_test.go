package internal

import (
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
)

func init() {
	// 工具测试里会调 LogDebug，确保默认 logger 存在，避免 nil panic。
	slog.SetDefault(slog.New(slog.NewTextHandler(&strings.Builder{}, nil)))
}

// 首选格式：<|GLMML|tool_calls> 包装。
func TestGLMMLToolCallFormat(t *testing.T) {
	text := `<|GLMML|tool_calls><|GLMML|invoke name="get_weather"><|GLMML|parameter name="city"><![CDATA[北京]]></|GLMML|parameter></|GLMML|invoke></|GLMML|tool_calls>`
	calls := ExtractToolInvocations(text)
	if len(calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(calls))
	}
	if calls[0].Function.Name != "get_weather" {
		t.Fatalf("expected name get_weather, got %q", calls[0].Function.Name)
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(calls[0].Function.Arguments), &args); err != nil {
		t.Fatalf("arguments not valid JSON: %v (raw=%s)", err, calls[0].Function.Arguments)
	}
	if args["city"] != "北京" {
		t.Fatalf("expected city=北京, got %v", args["city"])
	}
	if calls[0].ID == "" {
		t.Fatal("expected non-empty call id")
	}
}

// 多参数 + 多次调用。
func TestGLMMLMultiParam(t *testing.T) {
	text := `<|GLMML|tool_calls>
<|GLMML|invoke name="search"><|GLMML|parameter name="query"><![CDATA[GLM-5.2]]></|GLMML|parameter><|GLMML|parameter name="limit"><![CDATA[5]]></|GLMML|parameter></|GLMML|invoke>
<|GLMML|invoke name="read_file"><|GLMML|parameter name="path"><![CDATA[/tmp/a.txt]]></|GLMML|parameter></|GLMML|invoke>
</|GLMML|tool_calls>`
	calls := ExtractToolInvocations(text)
	if len(calls) != 2 {
		t.Fatalf("expected 2 calls, got %d", len(calls))
	}
	if calls[0].Function.Name != "search" {
		t.Fatalf("expected first call search, got %q", calls[0].Function.Name)
	}
	if calls[1].Function.Name != "read_file" {
		t.Fatalf("expected second call read_file, got %q", calls[1].Function.Name)
	}
	var args map[string]any
	_ = json.Unmarshal([]byte(calls[0].Function.Arguments), &args)
	if args["query"] != "GLM-5.2" {
		t.Fatalf("expected query=GLM-5.2, got %v", args["query"])
	}
}

// 兼容 legacy XML：<tool_calls><invoke name="X"><parameter name="p">v</parameter>...
// GLM 偶尔仍会吐旧格式，必须兼容。
func TestLegacyXMLStillWorks(t *testing.T) {
	text := `<tool_calls><invoke name="Bash"><parameter name="command">pwd</parameter><parameter name="description">show cwd</parameter></invoke></tool_calls>`
	calls := ExtractToolInvocations(text)
	if len(calls) != 1 {
		t.Fatalf("expected 1 legacy call, got %d", len(calls))
	}
	if calls[0].Function.Name != "Bash" {
		t.Fatalf("expected name Bash, got %q", calls[0].Function.Name)
	}
}

// 回退到 reasoning：content 里没有，思考链里有。
func TestReasoningFallback(t *testing.T) {
	content := "现在我来调用工具帮您查询天气。"
	reasoning := "<|GLMML|tool_calls><|GLMML|invoke name=\"get_weather\"><|GLMML|parameter name=\"city\"><![CDATA[上海]]></|GLMML|parameter></|GLMML|invoke></|GLMML|tool_calls>"
	calls := ExtractToolInvocationsWithFallback(content, reasoning)
	if len(calls) != 1 {
		t.Fatalf("expected 1 call from reasoning fallback, got %d", len(calls))
	}
	if calls[0].Function.Name != "get_weather" {
		t.Fatalf("expected get_weather, got %q", calls[0].Function.Name)
	}
}

// RemoveToolJSONContent 应剥离整段标记，保留自然语言。
func TestRemoveToolJSONContent(t *testing.T) {
	text := "好的，我来执行。<|GLMML|tool_calls><|GLMML|invoke name=\"ls\"><|GLMML|parameter name=\"dir\"><![CDATA[.]]></|GLMML|parameter></|GLMML|invoke></|GLMML|tool_calls> 已完成。"
	cleaned := RemoveToolJSONContent(text)
	if strings.Contains(cleaned, "GLMML") || strings.Contains(cleaned, "tool_calls") {
		t.Fatalf("expected markup stripped, got %q", cleaned)
	}
	if !strings.Contains(cleaned, "好的，我来执行。") || !strings.Contains(cleaned, "已完成。") {
		t.Fatalf("expected natural text preserved, got %q", cleaned)
	}
}

// RemoveToolJSONContent 兼容 legacy 格式。
func TestRemoveToolJSONContentLegacy(t *testing.T) {
	text := "结果如下：<tool_calls><invoke name=\"x\"><parameter name=\"a\">1</parameter></invoke></tool_calls>"
	cleaned := RemoveToolJSONContent(text)
	if strings.Contains(cleaned, "tool_calls") {
		t.Fatalf("expected legacy markup stripped, got %q", cleaned)
	}
}
