import type { Metadata } from "next";
import { Geist, Geist_Mono } from "next/font/google";
import "./globals.css";
import { Providers } from "./providers";

const geistSans = Geist({ variable: "--font-geist-sans", subsets: ["latin"] });
const geistMono = Geist_Mono({ variable: "--font-geist-mono", subsets: ["latin"] });

export const metadata: Metadata = {
  title: "RAG Admin",
  description: "Admin UI for the RAG platform",
};

// Resolves the persisted/system theme to a `dark` class on <html> before
// React hydrates, so there's no light-then-dark flash. Mirrors the guarded
// localStorage read in ThemeToggle; kept in sync by hand since it must run
// as a plain inline script, not a component.
const THEME_INIT_SCRIPT = `(function(){try{var t=localStorage.getItem("adminui.theme");var d=t==="dark"||(t!=="light"&&window.matchMedia("(prefers-color-scheme: dark)").matches);document.documentElement.classList.toggle("dark",d);}catch(e){}})();`;

export default function RootLayout({ children }: LayoutProps<"/">) {
  return (
    <html lang="en" suppressHydrationWarning>
      <head>
        <script suppressHydrationWarning dangerouslySetInnerHTML={{ __html: THEME_INIT_SCRIPT }} />
      </head>
      <body
        className={`${geistSans.variable} ${geistMono.variable} min-h-full max-w-[100vw] overflow-x-hidden bg-bg font-sans text-fg antialiased`}
      >
        <Providers>{children}</Providers>
      </body>
    </html>
  );
}
