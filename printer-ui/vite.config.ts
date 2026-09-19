import { defineConfig } from 'vite'
import { svelte } from '@sveltejs/vite-plugin-svelte'
import tailwindcss from '@tailwindcss/vite'
import { viteSingleFile } from 'vite-plugin-singlefile'

// The printer page: what a browser sees at a printer's own address
// (https://ipp.<zone>/p/<printer>, the https twin of its ipps:// URI). Like
// continuation-ui it is NOT served from a static route — the ipp head
// returns the built index.html for a GET on a printer that exists, with the
// printer's details injected where the <!--txco:printer--> marker sits.
// viteSingleFile inlines all JS+CSS so the page is one self-contained file
// go:embed bakes into the binary.
export default defineConfig({
    plugins: [svelte(), tailwindcss(), viteSingleFile()],
    build: {
        outDir: '../chassis/server/personality/ipp/ui/dist',
        emptyOutDir: true,
    },
    base: './',
    server: {
        port: 6163,
        strictPort: true,
    },
})
