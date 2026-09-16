import { ROLES, type Member, type Role } from "@/lib/members";

type MembersTableProps = {
  members: Member[];
  onSetRole: (member: Member, role: Role) => void;
  onRemove: (member: Member) => void;
  // busyId disables the row's controls while a mutation for that member runs.
  busyId?: string | null;
};

export function MembersTable({ members, onSetRole, onRemove, busyId }: MembersTableProps) {
  if (members.length === 0) {
    return (
      <div className="mt-6 flex flex-col items-center gap-3 rounded-2xl border border-dashed border-border px-6 py-16 text-center">
        <h2 className="text-lg font-semibold tracking-tight text-fg">No members yet</h2>
        <p className="max-w-sm text-sm text-fg-muted">
          Add a member by email to give them access to this tenant.
        </p>
      </div>
    );
  }

  return (
    <div className="mt-6 overflow-x-auto rounded-2xl border border-border">
      <table className="w-full border-collapse text-left text-sm">
        <thead>
          <tr className="border-b border-border text-xs uppercase tracking-wide text-fg-muted">
            <th className="px-4 py-3 font-medium">Email</th>
            <th className="px-4 py-3 font-medium">Role</th>
            <th className="px-4 py-3 font-medium">
              <span className="sr-only">Actions</span>
            </th>
          </tr>
        </thead>
        <tbody>
          {members.map((m) => {
            const busy = busyId === m.userId;
            return (
              <tr key={m.userId} className="border-b border-border last:border-b-0 align-top">
                <td className="px-4 py-3 font-medium text-fg">{m.email}</td>
                <td className="px-4 py-3">
                  <label className="sr-only" htmlFor={`role-${m.userId}`}>
                    Role for {m.email}
                  </label>
                  <select
                    id={`role-${m.userId}`}
                    value={m.role}
                    disabled={busy}
                    onChange={(e) => onSetRole(m, e.target.value as Role)}
                    className="h-8 rounded-md border border-border bg-bg px-2 text-xs font-medium text-fg transition-colors hover:border-border-strong focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent-text focus-visible:ring-offset-2 focus-visible:ring-offset-bg disabled:cursor-not-allowed disabled:opacity-50"
                  >
                    {ROLES.map((role) => (
                      <option key={role} value={role}>
                        {role}
                      </option>
                    ))}
                  </select>
                </td>
                <td className="px-4 py-3">
                  <div className="flex items-center justify-end">
                    <button
                      type="button"
                      onClick={() => onRemove(m)}
                      disabled={busy}
                      className="h-8 rounded-md border border-border px-2.5 text-xs font-medium text-danger-text transition-colors hover:border-danger-text hover:bg-danger-tint focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent-text focus-visible:ring-offset-2 focus-visible:ring-offset-bg disabled:cursor-not-allowed disabled:opacity-50"
                    >
                      Remove
                    </button>
                  </div>
                </td>
              </tr>
            );
          })}
        </tbody>
      </table>
    </div>
  );
}
