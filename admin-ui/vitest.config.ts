import { defineConfig } from 'vitest/config'
import { svelte } from '@sveltejs/vite-plugin-svelte'
import { svelteTesting } from '@testing-library/svelte/vite'

// Standalone test config, kept separate from vite.config.ts so the
// production build (which writes into the chassis embed dir) is never
// perturbed by test settings. Vitest auto-loads this. The svelteTesting
// plugin wires the `browser` resolve condition + auto-cleanup between
// tests.
export default defineConfig({
    plugins: [svelte(), svelteTesting()],
    test: {
        environment: 'jsdom',
        setupFiles: ['./vitest-setup.ts'],
        include: ['src/**/*.{test,spec}.ts'],
    },
})
