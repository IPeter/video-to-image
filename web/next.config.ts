import type { NextConfig } from "next";

// One image serves every domain: nothing domain-specific is read at build time. The
// domain key and API URL come from process.env at request time (lib/runtime-config.ts).
const nextConfig: NextConfig = {
  output: "standalone",
  poweredByHeader: false,
  reactStrictMode: true,
  async headers() {
    return [
      {
        source: "/:path*",
        headers: [
          { key: "X-Content-Type-Options", value: "nosniff" },
          { key: "X-Frame-Options", value: "DENY" },
          // Job URLs are the access handle for a result: never leak the path cross-origin.
          { key: "Referrer-Policy", value: "strict-origin-when-cross-origin" },
        ],
      },
    ];
  },
};

export default nextConfig;
