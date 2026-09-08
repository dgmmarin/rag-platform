"use client";

import Link from "next/link";
import { usePathname } from "next/navigation";
import { useAuth } from "@/lib/auth";
import { TenantProvider } from "@/lib/tenant";
import { TenantSwitcher } from "@/components/TenantSwitcher";
import { ThemeToggle } from "@/components/ThemeToggle";
import { RequireAuth } from "@/components/RequireAuth";
import { NAV_SECTIONS } from "@/lib/nav";

function Shell({ children }: { children: React.ReactNode }) {
  const { me, logout } = useAuth();
  const pathname = usePathname();

  return (
    <div className="flex min-h-screen flex-col bg-bg text-fg">
      <header className="sticky top-0 z-10 flex h-14 shrink-0 items-center gap-3 border-b border-border bg-bg-elevated px-4 sm:px-6">
        <span className="flex items-center gap-2 text-sm font-semibold tracking-tight text-fg">
          <span className="flex h-6 w-6 items-center justify-center rounded-md bg-accent text-xs font-semibold text-accent-fg">
            R
          </span>
          RAG Admin
        </span>

        <div className="flex-1" />

        <TenantSwitcher />
        <ThemeToggle />

        <div className="flex items-center gap-3 border-l border-border pl-3">
          <span className="text-sm text-fg-muted">{me?.user.email}</span>
          <button
            type="button"
            onClick={() => void logout()}
            className="h-9 rounded-md border border-border bg-bg-elevated px-3 text-sm font-medium text-fg transition-colors hover:border-border-strong hover:bg-bg-subtle focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent-text focus-visible:ring-offset-2 focus-visible:ring-offset-bg-elevated"
          >
            Log out
          </button>
        </div>
      </header>

      <div className="flex flex-1">
        <aside className="w-56 shrink-0 border-r border-border px-3 py-6">
          <nav aria-label="Sections" className="flex flex-col gap-0.5">
            {NAV_SECTIONS.map((s) => {
              const href = `/admin/${s.slug}`;
              const active = pathname === href || pathname.startsWith(`${href}/`);
              return (
                <Link
                  key={s.slug}
                  href={href}
                  aria-current={active ? "page" : undefined}
                  className={
                    active
                      ? "rounded-md bg-accent-tint px-3 py-2 text-sm font-medium text-accent-text"
                      : "rounded-md px-3 py-2 text-sm font-medium text-fg-muted transition-colors hover:bg-bg-subtle hover:text-fg"
                  }
                >
                  {s.label}
                </Link>
              );
            })}
          </nav>
        </aside>

        <main className="flex-1 px-4 py-8 sm:px-8">
          <div className="mx-auto max-w-5xl">{children}</div>
        </main>
      </div>
    </div>
  );
}

export default function AdminLayout({ children }: LayoutProps<"/admin">) {
  // The login page lives under /admin/login but must render bare: it has no
  // session to guard yet, and RequireAuth would just redirect it to itself.
  const pathname = usePathname();
  if (pathname === "/admin/login") return <>{children}</>;

  return (
    <RequireAuth>
      <TenantProvider>
        <Shell>{children}</Shell>
      </TenantProvider>
    </RequireAuth>
  );
}
