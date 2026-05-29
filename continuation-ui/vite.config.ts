import { defineConfig } from 'vite'
import { svelte } from '@sveltejs/vite-plugin-svelte'
import tailwindcss from '@tailwindcss/vite'
import { viteSingleFile } from 'vite-plugin-singlefile'

// The continuation "please wait" page. Unlike admin-ui this is NOT served
// from a static route — the chassis returns the built index.html as the
// body of the continuation poll response (so the browser stays on
// ?_txc.continuation=<rcid> and keeps polling). viteSingleFile inlines all
// JS+CSS so the whole page is one self-contained file go:embed can bake
// into the binary. base "./" keeps any (inlined) refs relative.
export default defineConfig({
    plugins: [svelte(), tailwindcss(), viteSingleFile()],
    build: {
        outDir: '../chassis/server/continuation/ui/dist',
        emptyOutDir: true,
    },
    base: './',
    server: {
        port: 6162,
        strictPort: true,
        proxy: {
            '/healthz': 'http://localhost:8080',
        },
    },
})
