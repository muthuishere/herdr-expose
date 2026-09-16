import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'
import { VitePWA } from 'vite-plugin-pwa'

// The SPA is embedded into the Go binary via `go:embed all:web/dist` and served
// from the site root, so base stays './'-free absolute ('/').
export default defineConfig({
  plugins: [
    react(),
    VitePWA({
      registerType: 'autoUpdate',
      // Offline SHELL ONLY. A terminal is inherently online: we precache the app
      // chrome so the "disconnected" screen can paint, and we never touch /v1/*.
      injectRegister: 'auto',
      includeAssets: ['icons/favicon.svg', 'icons/apple-touch-icon.png'],
      manifest: {
        name: 'Herdr Expose',
        short_name: 'Herdr',
        description: 'Your Herdr panes and coding agents, on your phone.',
        theme_color: '#0b0d10',
        background_color: '#0b0d10',
        display: 'standalone',
        orientation: 'any',
        scope: '/',
        start_url: '/',
        icons: [
          { src: '/icons/icon-192.png', sizes: '192x192', type: 'image/png' },
          { src: '/icons/icon-512.png', sizes: '512x512', type: 'image/png' },
          {
            src: '/icons/maskable-512.png',
            sizes: '512x512',
            type: 'image/png',
            purpose: 'maskable',
          },
        ],
      },
      workbox: {
        globPatterns: ['**/*.{js,css,html,svg,png,woff2}'],
        // Never let the SW answer for the API or the websocket upgrade path.
        navigateFallbackDenylist: [/^\/v1\//, /^\/healthz$/],
        runtimeCaching: [],
        cleanupOutdatedCaches: true,
        clientsClaim: true,
        skipWaiting: true,
      },
      devOptions: { enabled: false },
    }),
  ],
  build: {
    target: 'es2020',
    sourcemap: false,
    rollupOptions: {
      output: {
        manualChunks: {
          // xterm is only needed once a pane is opened; keep it off the shell path.
          xterm: [
            '@xterm/xterm',
            '@xterm/addon-canvas',
            '@xterm/addon-webgl',
            '@xterm/addon-unicode11',
          ],
        },
      },
    },
  },
  server: {
    port: 5173,
    // In non-mock dev, proxy to a locally running herdr-expose serve.
    proxy: {
      '/v1': { target: 'http://127.0.0.1:21118', ws: true, changeOrigin: false },
      '/healthz': { target: 'http://127.0.0.1:21118' },
    },
  },
})
