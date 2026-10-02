import type { Metadata, Viewport } from 'next';
import './globals.css';

export const metadata: Metadata = {
  title: 'O',
  description: '以插件扩展能力的本地 Agent 工作台。',
  manifest: '/manifest.webmanifest',
  appleWebApp: { capable: true, title: 'O', statusBarStyle: 'default' },
  icons: { icon: '/favicon.svg', apple: '/icons/apple-touch-icon.png' },
};

export const viewport: Viewport = {
  width: 'device-width',
  initialScale: 1,
  viewportFit: 'cover',
  interactiveWidget: 'resizes-content',
  themeColor: '#f4f3ee',
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
