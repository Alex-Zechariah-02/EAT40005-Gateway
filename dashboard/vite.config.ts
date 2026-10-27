import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'
import { adminURL, developmentPort } from '../e2e/ports.ts'

// The dashboard is served by the gateway's admin listener at /, which serves
// only index.html and flat files under /assets/.
export default defineConfig({
  base: '/',
  plugins: [react()],
  server: {
    host: 'localhost',
    port: developmentPort(5173),
    strictPort: true,
    proxy: {
      '/admin': {
        target: adminURL,
        changeOrigin: true,
        configure: (proxy) => {
          proxy.on('proxyReq', (proxyRequest) => {
            if (proxyRequest.getHeader('origin')) proxyRequest.setHeader('origin', adminURL)
          })
        },
      },
    },
  },
  build: {
    outDir: 'dist',
    emptyOutDir: true,
    // The page's content policy allows only its own script and style files,
    // so nothing may be inlined.
    assetsInlineLimit: 0,
    modulePreload: { polyfill: false },
    sourcemap: false,
  },
})
