"use client";

import { useState, type FormEvent } from "react";
import { useRouter } from "next/navigation";
import { useAuth } from "@/lib/auth";
import { Unauthorized } from "@/lib/api";

function Spinner() {
  return (
    <svg
      aria-hidden="true"
      viewBox="0 0 24 24"
      fill="none"
      className="h-4 w-4 animate-spin"
    >
      <circle cx="12" cy="12" r="9" stroke="currentColor" strokeWidth="3" className="opacity-25" />
      <path
        d="M21 12a9 9 0 0 0-9-9"
        stroke="currentColor"
        strokeWidth="3"
        strokeLinecap="round"
        className="opacity-90"
      />
    </svg>
  );
}

const inputClasses =
  "h-10 rounded-md border border-border bg-bg px-3 text-sm text-fg placeholder:text-fg-subtle transition-colors hover:border-border-strong focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent-text focus-visible:ring-offset-2 focus-visible:ring-offset-bg-elevated";

export default function LoginPage() {
  const { login } = useAuth();
  const router = useRouter();
  const [email, setEmail] = useState("");
  const [password, setPassword] = useState("");
  const [error, setError] = useState<string | null>(null);
  const [submitting, setSubmitting] = useState(false);

  async function onSubmit(e: FormEvent) {
    e.preventDefault();
    setError(null);
    setSubmitting(true);
    try {
      await login(email, password);
      router.push("/admin");
    } catch (err) {
      if (err instanceof Unauthorized) setError("Invalid email or password.");
      else throw err;
    } finally {
      setSubmitting(false);
    }
  }

  return (
    <main className="relative flex min-h-screen items-center justify-center overflow-hidden bg-bg px-4 py-12">
      {/* Ambient depth: a soft accent glow behind the card, purely decorative. */}
      <div
        aria-hidden="true"
        className="pointer-events-none absolute inset-0 [background:radial-gradient(60%_50%_at_50%_0%,color-mix(in_srgb,var(--color-accent)_14%,transparent),transparent_70%)]"
      />

      <div className="relative w-full max-w-sm rounded-2xl border border-border bg-bg-elevated p-8 shadow-card">
        <div className="mb-8 flex flex-col items-center gap-4 text-center">
          <div className="flex items-center gap-2">
            <span className="flex h-7 w-7 items-center justify-center rounded-md bg-accent text-sm font-semibold text-accent-fg">
              R
            </span>
            <span className="text-sm font-semibold tracking-tight text-fg">RAG Admin</span>
          </div>
          <div>
            <h1 className="text-xl font-semibold tracking-tight text-fg">Sign in to your workspace</h1>
            <p className="mt-1 text-sm text-fg-muted">Use your email and password to continue</p>
          </div>
        </div>

        <form onSubmit={onSubmit} className="flex flex-col gap-4" noValidate>
          <div className="flex flex-col gap-1.5">
            <label htmlFor="email" className="text-sm font-medium text-fg">
              Email
            </label>
            <input
              id="email"
              type="email"
              autoComplete="username"
              value={email}
              onChange={(e) => setEmail(e.target.value)}
              required
              className={inputClasses}
            />
          </div>
          <div className="flex flex-col gap-1.5">
            <label htmlFor="password" className="text-sm font-medium text-fg">
              Password
            </label>
            <input
              id="password"
              type="password"
              autoComplete="current-password"
              value={password}
              onChange={(e) => setPassword(e.target.value)}
              required
              className={inputClasses}
            />
          </div>

          {error && (
            <p
              role="alert"
              className="rounded-md border border-danger-text/20 bg-danger-tint px-3 py-2 text-sm text-danger-text"
            >
              {error}
            </p>
          )}

          <button
            type="submit"
            disabled={submitting}
            className="mt-2 inline-flex h-10 items-center justify-center gap-2 rounded-md bg-accent text-sm font-medium text-accent-fg transition-colors hover:bg-accent-hover active:bg-accent-active disabled:cursor-not-allowed disabled:opacity-70 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent-text focus-visible:ring-offset-2 focus-visible:ring-offset-bg-elevated"
          >
            {submitting && <Spinner />}
            {submitting ? "Signing in…" : "Sign in"}
          </button>
        </form>

        <p className="mt-6 text-center text-sm">
          <a
            href="/bff/v1/auth/oidc/start"
            className="text-fg-muted underline-offset-4 transition-colors hover:text-accent-text hover:underline focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent-text focus-visible:ring-offset-2 focus-visible:ring-offset-bg-elevated"
          >
            Sign in with OIDC instead
          </a>
        </p>
      </div>
    </main>
  );
}
