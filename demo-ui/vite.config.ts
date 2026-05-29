import { defineConfig } from 'vite'
import { svelte } from '@sveltejs/vite-plugin-svelte'
import tailwindcss from '@tailwindcss/vite'

// The Svelte SPA at this directory ships into the chassis binary via
// go:embed. Vite writes the build into ../chassis/server/demo/ui/dist
// directly so there's a single source of truth — no copy step.
export default defineConfig({
    plugins: [svelte(), tailwindcss()],
    build: {
        outDir: '../chassis/server/demo/ui/dist',
        emptyOutDir: true,
        // The bundle is served from /demo/ when embedded; Vite emits
        // asset URLs as /demo/assets/... thanks to `base` below. In
        // dev (vite serve) base stays "/" so the proxy works.
    },
    base: process.env.NODE_ENV === 'production' ? '/demo/' : '/',
    server: {
        port: 6163,
        strictPort: true,
        proxy: {
            '/v1': 'http://localhost:8081',
            '/traces': 'http://localhost:8081',
            '/auth': 'http://localhost:8081',
            '/healthz': 'http://localhost:8081',
        },
    },
})
