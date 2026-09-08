import { NAV_SECTIONS } from "@/lib/nav";

function SectionIcon() {
  return (
    <svg aria-hidden="true" viewBox="0 0 24 24" fill="none" className="h-6 w-6 text-accent-text">
      <rect x="3.5" y="3.5" width="17" height="17" rx="4" stroke="currentColor" strokeWidth="1.5" />
      <path d="M8 12h8M8 8.5h8M8 15.5h5" stroke="currentColor" strokeWidth="1.5" strokeLinecap="round" />
    </svg>
  );
}

export default async function SectionPage({ params }: PageProps<"/admin/[section]">) {
  const { section } = await params;
  const label = NAV_SECTIONS.find((s) => s.slug === section)?.label ?? section;

  return (
    <div className="flex flex-col gap-1">
      <p className="font-mono text-xs uppercase tracking-wide text-fg-muted">/admin/{section}</p>
      <div className="mt-8 flex flex-col items-center gap-3 rounded-2xl border border-dashed border-border px-6 py-16 text-center">
        <span className="flex h-12 w-12 items-center justify-center rounded-xl bg-accent-tint">
          <SectionIcon />
        </span>
        <h1 className="text-lg font-semibold tracking-tight text-fg">{label}</h1>
        <p className="max-w-sm text-sm text-fg-muted">This screen is coming in a later 11.x story — there&rsquo;s nothing to show here yet.</p>
      </div>
    </div>
  );
}
