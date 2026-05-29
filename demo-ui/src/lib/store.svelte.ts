// Minimal Svelte 5 runes store for the demo. Module-level so
// every component (notably the vendored JsonView) sees the same
// instance. The only state it owns is the per-path JSON collapse map
// that JsonView reads/toggles; the demo is otherwise stateless
// (each Run builds its own request/response in component scope).
function createStore() {
    const state = $state({
        // Keyed by JsonView's dot-path (e.g. "._txc.web"); a present
        // `true` value means that node is collapsed. Mirrors admin-ui's
        // jsonCollapsed shape so the vendored JsonView works unchanged.
        jsonCollapsed: {} as Record<string, boolean>,
    })

    function toggleJsonCollapse(path: string) {
        if (state.jsonCollapsed[path]) {
            delete state.jsonCollapsed[path]
        } else {
            state.jsonCollapsed[path] = true
        }
    }

    return {
        state,
        toggleJsonCollapse,
    }
}

export const store = createStore()
