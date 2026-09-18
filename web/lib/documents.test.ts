import { describe, it, expect, vi } from "vitest";
import { listAllDocuments } from "./documents";

// listAllDocuments must follow next_cursor across pages so the admin table shows
// every document, not just the server's first page (the "50 in total" bug).
describe("listAllDocuments", () => {
  it("follows next_cursor and aggregates every page", async () => {
    const spy = vi
      .spyOn(globalThis, "fetch")
      .mockResolvedValueOnce(
        new Response(JSON.stringify({ items: [{ id: "a" }, { id: "b" }], next_cursor: "c1" }), { status: 200 }),
      )
      .mockResolvedValueOnce(
        new Response(JSON.stringify({ items: [{ id: "c" }] }), { status: 200 }),
      );

    const page = await listAllDocuments("t1");

    expect(page.items.map((d) => d.id)).toEqual(["a", "b", "c"]);
    expect(spy).toHaveBeenCalledTimes(2);
    // The first request asks for the server max page size; the second carries the cursor.
    expect(spy.mock.calls[0][0]).toContain("limit=200");
    expect(spy.mock.calls[1][0]).toContain("cursor=c1");
  });

  it("stops after a single page when there is no next_cursor", async () => {
    const spy = vi
      .spyOn(globalThis, "fetch")
      .mockResolvedValue(new Response(JSON.stringify({ items: [{ id: "only" }] }), { status: 200 }));

    const page = await listAllDocuments("t1");

    expect(page.items).toHaveLength(1);
    expect(spy).toHaveBeenCalledTimes(1);
  });
});
