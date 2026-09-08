import type { Metadata } from "next";
import "./globals.css";

export const metadata: Metadata = {
  title: "RAG Admin",
  description: "Admin UI for the RAG platform",
};

export default function RootLayout({ children }: LayoutProps<"/">) {
  return (
    <html lang="en">
      <body>{children}</body>
    </html>
  );
}
