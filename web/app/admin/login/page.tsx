"use client";

import { Suspense, useState, type FormEvent } from "react";
import { useRouter, useSearchParams } from "next/navigation";
import { useAuth, LoginFailed } from "@/lib/auth";
import { Unauthorized } from "@/lib/api";

// oidcErrorMessage maps the ?error=<code> the OIDC callback redirects with
// (ISSUE-0058) to a message for the operator. An unknown code falls back to a
// generic sign-in failure.
function oidcErrorMessage(code: string): string {
  switch (code) {
    case "invalid_state":
      return "Your sign-in session expired. Please try again.";
    case "email_unverified":
      return "Your email is not verified with your identity provider.";
    case "not_provisioned":
      return "No account exists for this identity. Contact your administrator.";
    case "unavailable":
      return "Single sign-on is temporarily unavailable. Please try again later.";
    default:
      return "Single sign-on failed. Please try again.";
  }
}

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
  // useSearchParams (read in LoginForm) must sit under a Suspense boundary, or
  // `next build` fails the route (Next.js requirement).
  return (
    <Suspense>
      <LoginForm />
    </Suspense>
  );
}

function LoginForm() {
  const { login } = useAuth();
  const router = useRouter();
  const searchParams = useSearchParams();
  const oidcError = searchParams?.get("error") ?? null;
  const [email, setEmail] = useState("");
  const [password, setPassword] = useState("");
  const [error, setError] = useState<string | null>(oidcError ? oidcErrorMessage(oidcError) : null);
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
      else if (err instanceof LoginFailed && err.status === 429)
        setError("Account temporarily locked. Try again later.");
      else setError("Something went wrong. Please try again.");
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

        {/* OIDC sign-in is a top-level browser navigation (not a fetch): the callback
            303-redirects into the SPA, or back here with ?error=<code> on failure (ISSUE-0058). */}
        <div className="mt-6 flex flex-col gap-3">
          <div className="flex items-center gap-3 text-xs text-fg-subtle">
            <span className="h-px flex-1 bg-border" />
            or
            <span className="h-px flex-1 bg-border" />
          </div>
          {/* A real full-page navigation, not next/link client routing: the browser
              must hit the BFF route handler so it follows the provider redirect and the
              callback's 303 back into the SPA. next/link cannot drive a route handler. */}
          {/* eslint-disable-next-line @next/next/no-html-link-for-pages */}
          <a
            href="/bff/v1/auth/oidc/start"
            className="inline-flex h-10 items-center justify-center rounded-md border border-border bg-bg text-sm font-medium text-fg transition-colors hover:border-border-strong hover:bg-bg-elevated focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent-text focus-visible:ring-offset-2 focus-visible:ring-offset-bg-elevated"
          >
            Sign in with OIDC
          </a>
        </div>
      </div>
    </main>
  );
}
