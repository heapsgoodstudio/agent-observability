// Next.js disallows rewrites/redirects/headers together with
// `output: 'export'` (static export can't run server-side routing), so the
// two modes this app needs are gated on NEXT_OUTPUT rather than both being
// declared at once:
//   - `just build` sets NEXT_OUTPUT=export: static export, copied into
//     internal/server/dist/ and served by the embedded Go binary.
//   - `just dev` (NEXT_OUTPUT unset): next dev proxies the Connect API
//     (and any other server paths) to a local `claude-obs serve`, which
//     defaults to 127.0.0.1:8427 (internal/server/server.go's DashPort).
const isExport = process.env.NEXT_OUTPUT === 'export';

/** @type {import('next').NextConfig} */
const nextConfig = {
  // images.unoptimized keeps Next's built-in image optimizer (which is what
  // actually calls sharp) unreachable: a static export has no server to run
  // it, and the embedded Go server does not provide one either.
  images: { unoptimized: true },
};

if (isExport) {
  nextConfig.output = 'export';
} else {
  nextConfig.rewrites = async () => [
    {
      source: '/claudeobs.v1.ObsService/:path*',
      destination: 'http://127.0.0.1:8427/claudeobs.v1.ObsService/:path*',
    },
  ];
}

export default nextConfig;
