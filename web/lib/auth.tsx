"use client";

import { createContext, useCallback, useContext, useEffect, useState, type ReactNode } from "react";
import { apiFetch, Unauthorized } from "./api";
import type { Me } from "./types";

type AuthState = {
  me: Me | null;
  loading: boolean;
  login: (email: string, password: string) => Promise<void>;
  logout: () => Promise<void>;
  refresh: () => Promise<void>;
};

const AuthContext = createContext<AuthState | null>(null);

// LoginFailed carries the HTTP status for a login attempt that failed for a
// reason other than bad credentials (apiFetch already throws Unauthorized for
// a 401), so the login page can show a distinct message for account lockout
// (429, ErrAccountLocked in middleware.go) vs. anything else (e.g. 500).
export class LoginFailed extends Error {
  status: number;
  constructor(status: number) {
    super(`login failed with status ${status}`);
    this.status = status;
  }
}

export function AuthProvider({ children }: { children: ReactNode }) {
  const [me, setMe] = useState<Me | null>(null);
  const [loading, setLoading] = useState(true);

  const refresh = useCallback(async () => {
    try {
      const res = await apiFetch("/v1/auth/me");
      setMe((await res.json()) as Me);
    } catch (err) {
      if (err instanceof Unauthorized) setMe(null);
      else throw err;
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => {
    (async () => {
      await refresh();
    })();
  }, [refresh]);

  const login = useCallback(
    async (email: string, password: string) => {
      const res = await apiFetch("/v1/auth/login", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ email, password }),
      });
      // apiFetch already throws Unauthorized for a 401 (bad credentials); anything
      // else non-OK (429 account-locked, 500, ...) is surfaced here so the login
      // page can show a message instead of silently proceeding to refresh().
      if (!res.ok) throw new LoginFailed(res.status);
      await refresh();
    },
    [refresh],
  );

  const logout = useCallback(async () => {
    try {
      await apiFetch("/v1/auth/logout", { method: "POST", csrfToken: me?.csrf_token });
    } catch (err) {
      // An already-expired session 401s on logout; there is nothing left to revoke,
      // so treat it the same as a successful logout instead of leaving an unhandled
      // rejection and skipping setMe(null) below.
      if (!(err instanceof Unauthorized)) throw err;
    } finally {
      setMe(null);
    }
  }, [me]);

  return <AuthContext.Provider value={{ me, loading, login, logout, refresh }}>{children}</AuthContext.Provider>;
}

export function useAuth(): AuthState {
  const ctx = useContext(AuthContext);
  if (!ctx) throw new Error("useAuth must be used within AuthProvider");
  return ctx;
}
