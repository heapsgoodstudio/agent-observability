import type { Metadata } from 'next';
import '@fontsource-variable/inter';
import '@fontsource/jetbrains-mono/400.css';
import { Shell } from '@/src/components/Shell';
import { THEME_STORAGE_KEY } from '@/lib/theme';
import './globals.css';

export const metadata: Metadata = {
  title: 'Agent Observability',
  description: 'Local plan allowances, usage and reported overage for Claude Code, Codex, Oh My Pi and opencode.',
};

// Runs before hydration paints, so the persisted theme (if an explicit
// light/dark choice was ever made) applies with no flash of the wrong
// theme. This is the one place data-theme is set outside a React effect;
// it can only ever run in the browser (an inline <script> tag is never
// executed by Node during the static-export prerender), and it mirrors only
// the dataset write half of lib/theme.ts's applyTheme, never the
// localStorage write: it must never persist a choice the user hasn't made.
// The stored key is interpolated from THEME_STORAGE_KEY so the two can't
// drift apart.
const THEME_INIT_SCRIPT = `try{var t=localStorage.getItem(${JSON.stringify(
  THEME_STORAGE_KEY,
)});document.documentElement.dataset.theme=t==='system'?'system':t==='light'?'light':'dark';}catch(e){}`;

export default function RootLayout({
  children,
}: Readonly<{ children: React.ReactNode }>) {
  return (
    // suppressHydrationWarning: the inline script above (and the theme
    // toggle) sets `data-theme` here client-side before paint;
    // without it React would warn about the attribute it didn't render
    // server-side.
    <html lang="en" suppressHydrationWarning>
      <head>
        <script dangerouslySetInnerHTML={{ __html: THEME_INIT_SCRIPT }} />
      </head>
      <body className="font-sans">
        <Shell>{children}</Shell>
      </body>
    </html>
  );
}
