// Upstream ragctl API the BFF proxies to. Local `next dev` gets :8091 from
// .env.development (repo .env RAGCTL_ADDR=:8091); the :8080 fallback is the
// docker-compose container port. Set RAGCTL_API_URL explicitly in other envs.
export const RAGCTL_API_URL = process.env.RAGCTL_API_URL ?? "http://localhost:8080";
