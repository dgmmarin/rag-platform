"use client";

import Link from "next/link";
import { usePathname } from "next/navigation";
import { useAuth } from "@/lib/auth";
import { TenantProvider } from "@/lib/tenant";
import { TenantSwitcher } from "@/components/TenantSwitcher";
import { RequireAuth } from "@/components/RequireAuth";
import { NAV_SECTIONS } from "@/lib/nav";
import styles from "./layout.module.css";

function Shell({ children }: { children: React.ReactNode }) {
  const { me, logout } = useAuth();

  return (
    <div className={styles.shell}>
      <header className={styles.topbar}>
        <span className={styles.email}>{me?.user.email}</span>
        <TenantSwitcher />
        <span className={styles.spacer} />
        <button type="button" onClick={() => void logout()}>
          Log out
        </button>
      </header>
      <div className={styles.body}>
        <nav className={styles.nav}>
          {NAV_SECTIONS.map((s) => (
            <Link key={s.slug} href={`/admin/${s.slug}`}>
              {s.label}
            </Link>
          ))}
        </nav>
        <main className={styles.main}>{children}</main>
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
