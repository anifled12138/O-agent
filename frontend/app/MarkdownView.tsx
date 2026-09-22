'use client';

import React, { useState } from 'react';
import { Copy, Check } from 'lucide-react';

interface MarkdownViewProps {
  content: string;
  className?: string;
}

async function copyToClipboard(text: string): Promise<boolean> {
  if (typeof window !== 'undefined' && window.oDesktop?.copyText) {
    try {
      const res = await window.oDesktop.copyText(text);
      if (res) return true;
    } catch {}
  }

  if (typeof navigator !== 'undefined' && navigator.clipboard && typeof navigator.clipboard.writeText === 'function') {
    try {
      await navigator.clipboard.writeText(text);
      return true;
    } catch {}
  }

  if (typeof document !== 'undefined') {
    try {
      const textArea = document.createElement('textarea');
      textArea.value = text;
      textArea.style.position = 'fixed';
      textArea.style.top = '0';
      textArea.style.left = '-9999px';
      textArea.style.width = '2em';
      textArea.style.height = '2em';
      textArea.style.padding = '0';
      textArea.style.border = 'none';
      textArea.style.outline = 'none';
      textArea.style.boxShadow = 'none';
      textArea.style.background = 'transparent';
      textArea.style.opacity = '0';
      textArea.setAttribute('readonly', '');
      document.body.appendChild(textArea);
      textArea.focus();
      textArea.select();
      textArea.setSelectionRange(0, text.length);
      const successful = document.execCommand('copy');
      document.body.removeChild(textArea);
      if (successful) return true;
    } catch {}
  }

  return false;
}

function CodeBlock({ language, code }: { language: string; code: string }) {
  const [copied, setCopied] = useState(false);

  const handleCopy = async () => {
    const success = await copyToClipboard(code);
    if (success) {
      setCopied(true);
      setTimeout(() => setCopied(false), 2000);
    }
  };

  return (
    <div className="md-code-container">
      <div className="md-code-header">
        <span className="md-code-lang">{language || 'text'}</span>
        <button
          type="button"
          className={"md-code-copy" + (copied ? " copied" : "")}
          onClick={handleCopy}
          title={copied ? "已复制" : "复制代码"}
          aria-label={copied ? "已复制" : "复制代码"}
        >
          {copied ? <Check size={13} strokeWidth={2.2} /> : <Copy size={13} strokeWidth={1.8} />}
        </button>
      </div>
      <pre className="md-code-body">
        <code>{code}</code>
      </pre>
    </div>
  );
}

export function MarkdownView({ content, className = '' }: MarkdownViewProps) {
  const renderContent = (text: string) => {
    const elements: React.ReactNode[] = [];
    const codeBlockRegex = /```([a-zA-Z0-9_+#.-]*)[^\n\r]*\r?\n([\s\S]*?)```/g;
    let lastIndex = 0;
    let match: RegExpExecArray | null;

    while ((match = codeBlockRegex.exec(text)) !== null) {
      const before = text.slice(lastIndex, match.index);
      if (before) {
        elements.push(renderTextWithFormatting(before, elements.length));
      }
      const lang = match[1].trim();
      const code = match[2].replace(/\r\n/g, '\n').replace(/\n$/, '');
      elements.push(
        <CodeBlock key={`code-${match.index}`} language={lang} code={code} />
      );
      lastIndex = match.index + match[0].length;
    }

    const remaining = text.slice(lastIndex);
    if (remaining) {
      const unclosedMatch = remaining.match(/^```([a-zA-Z0-9_+#.-]*)[^\n\r]*\r?\n([\s\S]*)$/);
      if (unclosedMatch) {
        const lang = unclosedMatch[1].trim();
        const code = unclosedMatch[2].replace(/\r\n/g, '\n');
        elements.push(
          <CodeBlock key={`code-stream-${lastIndex}`} language={lang} code={code} />
        );
      } else {
        elements.push(renderTextWithFormatting(remaining, elements.length));
      }
    }

    return elements;
  };

  const renderTextWithFormatting = (raw: string, baseKey: number) => {
    const lines = raw.split('\n');
    return (
      <div key={`chunk-${baseKey}`} className="md-prose">
        {lines.map((line, idx) => {
          const trimmed = line.trim();
          if (trimmed.startsWith('### ')) {
            return <h4 key={idx} className="md-h3">{renderInline(trimmed.slice(4))}</h4>;
          }
          if (trimmed.startsWith('## ')) {
            return <h3 key={idx} className="md-h2">{renderInline(trimmed.slice(3))}</h3>;
          }
          if (trimmed.startsWith('# ')) {
            return <h2 key={idx} className="md-h1">{renderInline(trimmed.slice(2))}</h2>;
          }
          if (trimmed.startsWith('> ')) {
            return <blockquote key={idx} className="md-quote">{renderInline(trimmed.slice(2))}</blockquote>;
          }
          const imgMatch = trimmed.match(/^!\[(.*?)\]\((.*?)\)$/);
          if (imgMatch) {
            return (
              <div key={idx} className="md-image-wrap">
                {/* Markdown may contain data URLs and arbitrary remote URLs. */}
                {/* eslint-disable-next-line @next/next/no-img-element */}
                <img src={imgMatch[2]} alt={imgMatch[1] || '图片'} className="md-image" />
              </div>
            );
          }
          if (trimmed.startsWith('- ') || trimmed.startsWith('* ')) {
            return (
              <div key={idx} className="md-li">
                <span className="md-bullet">•</span>
                <span>{renderInline(trimmed.slice(2))}</span>
              </div>
            );
          }
          const numMatch = trimmed.match(/^(\d+)\.\s+(.*)$/);
          if (numMatch) {
            return (
              <div key={idx} className="md-li">
                <span className="md-num">{numMatch[1]}.</span>
                <span>{renderInline(numMatch[2])}</span>
              </div>
            );
          }
          if (!trimmed) {
            return <div key={idx} className="md-spacer" />;
          }
          return <p key={idx} className="md-p">{renderInline(line)}</p>;
        })}
      </div>
    );
  };

  const renderInline = (str: string): React.ReactNode => {
    const tokens = str.split(/(!\[.*?\]\(.*?\)|`[^\\`]+`|\*\*[^*]+\*\*)/g);
    return tokens.map((token, i) => {
      const match = token.match(/^!\[(.*?)\]\((.*?)\)$/);
      if (match) {
        return (
          <span key={i} className="md-image-wrap inline">
            {/* Markdown may contain data URLs and arbitrary remote URLs. */}
            {/* eslint-disable-next-line @next/next/no-img-element */}
            <img src={match[2]} alt={match[1] || '图片'} className="md-image inline" />
          </span>
        );
      }
      if (token.startsWith('`') && token.endsWith('`') && token.length >= 2) {
        return <code key={i} className="md-inline-code">{token.slice(1, -1)}</code>;
      }
      if (token.startsWith('**') && token.endsWith('**') && token.length >= 4) {
        return <strong key={i} className="md-bold">{token.slice(2, -2)}</strong>;
      }
      return token;
    });
  };

  return (
    <div className={`markdown-view ${className}`.trim()}>
      {renderContent(content)}
    </div>
  );
}
