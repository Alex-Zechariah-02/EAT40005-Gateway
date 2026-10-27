import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'
import tailwindcss from '@tailwindcss/vite'
import { developmentPort, siteURL } from '../e2e/ports.ts'

// The gateway's data plane serves the app at / with its page routes and its
// flat assets at /assets/<name>, under a content policy that allows
// only the app's own scripts, styles, images and connections.
const gatewayPolicy =
  "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self'; connect-src 'self'; " +
  "base-uri 'none'; form-action 'none'; frame-ancestors 'none'"

export default defineConfig(({ mode }) => ({
  base: '/',
  plugins: [react(), tailwindcss()],
  server: {
    host: 'localhost',
    port: developmentPort(5174),
    strictPort: true,
    // Development only: API paths go to a running native gateway. In mock
    // mode the development stand-in in src/dev answers instead.
    proxy:
      mode === 'mock'
        ? undefined
        : { '^/((inventory|orders|reports|accounts)(/|$)|support/)': { target: siteURL } },
  },
  // `npx vite preview` serves a build with the gateway's own content policy, so
  // policy violations show up before the gateway serves the app.
  preview: { host: '127.0.0.1', port: 5175, strictPort: true, headers: { 'Content-Security-Policy': gatewayPolicy } },
  build: {
    outDir: mode === 'mock' ? 'dist-mock' : 'dist',
    emptyOutDir: true,
    // Nothing may be inlined: the policy allows only the app's own files.
    assetsInlineLimit: 0,
    modulePreload: { polyfill: false },
    sourcemap: false,
  },
}))
