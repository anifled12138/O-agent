import type { Metadata } from 'next';
import './globals.css';

export const metadata: Metadata = {
  title: 'O — 本地 Agent',
  description: '以插件扩展能力的本地 Agent 工作台。',
};

export default function RootLayout({
  children,
}: Readonly<{
  children: React.ReactNode;
}>) {
  return (
    <html lang="zh-CN">
      <body className="antialiased">
        {children}
      </body>
    </html>
  );
}
