import { NAV_SECTIONS } from "@/lib/nav";

export default async function SectionPage({ params }: PageProps<"/admin/[section]">) {
  const { section } = await params;
  const label = NAV_SECTIONS.find((s) => s.slug === section)?.label ?? section;
  return <p>{label}: coming in 11.x</p>;
}
