import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'
import { adminStandIn } from './src/dev/stand-in'

// The dashboard is served by the gateway's admin listener at /, which serves
// only index.html and flat files under /assets/. In mock mode (development
// only) a stand-in answers selected Admin API endpoints; everything else
// still goes to the real gateway.
export default defineConfig(({ mode }) => ({
  base: '/',
  plugins: mode === 'mock' ? [react(), adminStandIn()] : [react()],
  server: {
    host: 'localhost',
    port: 5173,
    strictPort: true,
    proxy: {
      '/admin': {
        target: 'http://127.0.0.1:9440',
        changeOrigin: true,
        configure: (proxy) => {
          proxy.on('proxyReq', (proxyRequest) => {
            if (proxyRequest.getHeader('origin')) proxyRequest.setHeader('origin', 'http://127.0.0.1:9440')
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
}))
