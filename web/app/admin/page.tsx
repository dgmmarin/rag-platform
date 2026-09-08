import { redirect } from "next/navigation";
import { NAV_SECTIONS } from "@/lib/nav";

// Bare /admin has nothing of its own to show — send it to the first nav
// section. Stays inside the guarded shell (layout.tsx only bypasses
// /admin/login), so this only fires for an authenticated user.
export default function AdminIndexPage() {
  redirect(`/admin/${NAV_SECTIONS[0].slug}`);
}
