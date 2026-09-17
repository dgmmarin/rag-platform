export type NavSection = { slug: string; label: string };

export const NAV_SECTIONS: NavSection[] = [
  { slug: "sources", label: "Sources" },
  { slug: "jobs", label: "Jobs" },
  { slug: "documents", label: "Documents" },
  { slug: "members", label: "Members" },
  { slug: "keys", label: "API Keys" },
  { slug: "settings", label: "Settings" },
  { slug: "query", label: "Query" },
  { slug: "eval", label: "Eval" },
];

// Platform-admin-only sections (STORY-11.7). Shown only when me.is_platform_admin;
// the server enforces the real gate (RequirePlatformAdmin) regardless.
export const PLATFORM_NAV_SECTIONS: NavSection[] = [{ slug: "tenants", label: "Tenants" }];
