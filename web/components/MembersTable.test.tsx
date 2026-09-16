import { describe, it, expect, vi } from "vitest";
import { render, screen, fireEvent } from "@testing-library/react";

import { MembersTable } from "./MembersTable";
import type { Member } from "@/lib/members";

function makeMember(over: Partial<Member> = {}): Member {
  return {
    userId: "u1",
    email: "ada@example.com",
    role: "admin",
    ...over,
  };
}

describe("MembersTable", () => {
  it("renders a row per member with email and a role select", () => {
    const members = [
      makeMember({ userId: "u1", email: "ada@example.com", role: "owner" }),
      makeMember({ userId: "u2", email: "grace@example.com", role: "viewer" }),
    ];

    render(<MembersTable members={members} onSetRole={vi.fn()} onRemove={vi.fn()} />);

    expect(screen.getByText("ada@example.com")).toBeInTheDocument();
    expect(screen.getByText("grace@example.com")).toBeInTheDocument();

    const selects = screen.getAllByRole("combobox");
    expect(selects).toHaveLength(2);
    expect(selects[0]).toHaveValue("owner");
    expect(selects[1]).toHaveValue("viewer");
  });

  it("shows an empty state and no rows when there are no members", () => {
    render(<MembersTable members={[]} onSetRole={vi.fn()} onRemove={vi.fn()} />);

    expect(screen.getByText(/no members yet/i)).toBeInTheDocument();
    expect(screen.queryByRole("combobox")).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /remove/i })).not.toBeInTheDocument();
  });

  it("calls onSetRole with the member and the new role when the select changes", () => {
    const onSetRole = vi.fn();
    const member = makeMember({ userId: "u1", email: "ada@example.com", role: "admin" });

    render(<MembersTable members={[member]} onSetRole={onSetRole} onRemove={vi.fn()} />);

    fireEvent.change(screen.getByRole("combobox"), { target: { value: "editor" } });

    expect(onSetRole).toHaveBeenCalledWith(member, "editor");
  });

  it("calls onRemove with the member when the remove action is used", () => {
    const onRemove = vi.fn();
    const member = makeMember({ userId: "u1", email: "ada@example.com" });

    render(<MembersTable members={[member]} onSetRole={vi.fn()} onRemove={onRemove} />);

    fireEvent.click(screen.getByRole("button", { name: /remove/i }));

    expect(onRemove).toHaveBeenCalledWith(member);
  });

  it("disables the role select and remove action for the busy row only", () => {
    const members = [
      makeMember({ userId: "u1", email: "ada@example.com" }),
      makeMember({ userId: "u2", email: "grace@example.com" }),
    ];

    render(
      <MembersTable members={members} onSetRole={vi.fn()} onRemove={vi.fn()} busyId="u1" />,
    );

    const selects = screen.getAllByRole("combobox");
    const removes = screen.getAllByRole("button", { name: /remove/i });
    expect(selects[0]).toBeDisabled();
    expect(removes[0]).toBeDisabled();
    expect(selects[1]).not.toBeDisabled();
    expect(removes[1]).not.toBeDisabled();
  });
});
