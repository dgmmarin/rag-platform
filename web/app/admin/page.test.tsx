import { describe, it, expect, vi, beforeEach } from "vitest";

const redirectMock = vi.fn();
vi.mock("next/navigation", () => ({ redirect: (path: string) => redirectMock(path) }));

import AdminIndexPage from "./page";
import { NAV_SECTIONS } from "@/lib/nav";

beforeEach(() => {
  redirectMock.mockReset();
});

describe("AdminIndexPage", () => {
  it("redirects /admin to the first nav section", () => {
    // Server component with no props/hooks — calling it directly exercises the
    // same body Next would run; `redirect` is mocked so it doesn't throw
    // Next's real (non-jsdom-friendly) NEXT_REDIRECT control-flow signal.
    AdminIndexPage();
    expect(redirectMock).toHaveBeenCalledWith(`/admin/${NAV_SECTIONS[0].slug}`);
    expect(redirectMock).toHaveBeenCalledWith("/admin/sources");
  });
});
