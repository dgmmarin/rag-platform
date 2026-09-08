import type { NextConfig } from "next";

const nextConfig: NextConfig = {
  // Standalone output: a self-contained server bundle (deps trimmed to what's
  // actually used) so the runtime Docker stage is just that folder + node.
  output: "standalone",
};

export default nextConfig;
