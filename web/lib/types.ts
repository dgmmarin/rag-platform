export type Me = {
  user: { id: string; email: string };
  is_platform_admin: boolean;
  memberships: { tenant_id: string; slug: string; name: string; role: string }[];
  csrf_token: string;
};
