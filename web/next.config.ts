import type { NextConfig } from "next";

// The console is a fully client-side SPA. In production (the Docker build) it is
// statically exported (`output: "export"` -> `out/`) and served by the Go
// backend. No Node.js or reverse proxy is required in production.
//
// In dev (`next dev`), we instead proxy those same paths to the backend via
// rewrites so the browser sees a single same-origin app. The two modes are kept
// separate because `output: export` is incompatible with rewrites.
const isDev = process.env.NODE_ENV === "development";

const nextConfig: NextConfig = {
  reactStrictMode: true,
  ...(isDev
    ? {
        async rewrites() {
          const backend = process.env.BACKEND_ORIGIN || "http://localhost:8080";
          return [
            { source: "/api/:path*", destination: `${backend}/api/:path*` },
            { source: "/sse", destination: `${backend}/sse` },
            { source: "/yjs/:path*", destination: `${backend}/yjs/:path*` },
            { source: "/files/:path*", destination: `${backend}/files/:path*` },
          ];
        },
      }
    : {
        output: "export",
        // The Next.js image optimizer needs a server; static export can't use it.
        images: { unoptimized: true },
      }),
};

export default nextConfig;
