package agent

import (
	"testing"
)

func TestTitleCleaning(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{
			input:    "【代码审查】重构登录鉴权模块",
			expected: "重构登录鉴权模块",
		},
		{
			input:    "[架构设计] 本地Agent插件机制",
			expected: "本地Agent插件机制",
		},
		{
			input:    "“【问题排查】修复连接超时”",
			expected: "修复连接超时",
		},
		{
			input:    "```\n【性能优化】提升并发吞吐量\n```",
			expected: "提升并发吞吐量",
		},
		{
			input:    "标题：开发智能会话标题与编辑插件",
			expected: "开发智能会话标题与编辑插件",
		},
		{
			input:    "分析一下为什么内存会暴涨",
			expected: "分析一下为什么内存会暴涨",
		},
		{
			input:    "如何编写单元测试用例",
			expected: "如何编写单元测试用例",
		},
	}

	for _, tt := range tests {
		got := cleanTitle(tt.input)
		if got != tt.expected {
			t.Errorf("cleanTitle(%q) = %q, want %q", tt.input, got, tt.expected)
		}
	}
}

func TestFallbackTitle(t *testing.T) {
	title := fallbackTitle("帮我检查这个项目的架构和插件系统")
	if title != "检查这个项目的架构和插件系统" {
		t.Errorf("expected clean fallback title without fillers, got %q", title)
	}

	title2 := fallbackTitle("帮我实现一个智能会话标题插件")
	if title2 != "智能会话标题插件" {
		t.Errorf("expected clean fallback title, got %q", title2)
	}
}
