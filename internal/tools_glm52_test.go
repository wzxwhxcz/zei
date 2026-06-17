package internal

import (
	"log/slog"
	"os"
	"testing"
)

func init() {
	// 测试环境初始化 logger，避免 LogDebug 命中 nil logger panic
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})))
}

func TestGLM52ToolCallFormat(t *testing.T) {
	// GLM-5.2 自创格式：<tool_injection><tool_call><name><parameters><parameter name="x">
	input := `<tool_injection>
<tool_call>
<name>search_web</name>
<parameters>
<parameter name="query">latest news about AI</parameter>
</parameters>
</tool_call>
</tool_injection>`
	calls := ExtractToolInvocations(input)
	if len(calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(calls))
	}
	if calls[0].Function.Name != "search_web" {
		t.Fatalf("name=%s want search_web", calls[0].Function.Name)
	}
	want := `{"query":"latest news about AI"}`
	if calls[0].Function.Arguments != want {
		t.Fatalf("args=%q want %q", calls[0].Function.Arguments, want)
	}
}

func TestGLM52MultiParam(t *testing.T) {
	input := `<tool_call>
<name>calculate</name>
<parameters>
<parameter name="expression">2 + 2</parameter>
<parameter name="precision">high</parameter>
</parameters>
</tool_call>`
	calls := ExtractToolInvocations(input)
	if len(calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(calls))
	}
	if calls[0].Function.Name != "calculate" {
		t.Fatalf("name=%s want calculate", calls[0].Function.Name)
	}
	// JSON 对象，键顺序不保证，检查两个 key 都在
	args := calls[0].Function.Arguments
	if !containsStr(args, `"expression":"2 + 2"`) {
		t.Fatalf("missing expression in %s", args)
	}
	if !containsStr(args, `"precision":"high"`) {
		t.Fatalf("missing precision in %s", args)
	}
}

func TestStandardStillWorks(t *testing.T) {
	input := `<tool_calls>
  <tool_call id="call_1">
    <name>get_weather</name>
    <arguments><![CDATA[{"location":"Tokyo"}]]></arguments>
  </tool_call>
</tool_calls>`
	calls := ExtractToolInvocations(input)
	if len(calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(calls))
	}
	if calls[0].Function.Name != "get_weather" {
		t.Fatalf("name=%s", calls[0].Function.Name)
	}
	if calls[0].Function.Arguments != `{"location":"Tokyo"}` {
		t.Fatalf("args=%s", calls[0].Function.Arguments)
	}
	if calls[0].ID != "call_1" {
		t.Fatalf("id=%s want call_1", calls[0].ID)
	}
}

func containsStr(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
