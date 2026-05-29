# thanks-computer admin UI

A small Svelte 5 + Vite + TailwindCSS SPA for browsing the op stack on a
running chassis. The built bundle is embedded into the chassis binary
via `go:embed` and served at `/admin/` on the admin port.

## Dev loop

In two terminals:

```bash
# Terminal 1: run the chassis (any example works)
cd ../examples/quickstart-hello-world
txco dev

# Terminal 2: run the Vite dev server
cd admin-ui
npm install        # first time only
npm run dev
# → http://localhost:6161 with /v1/* and /traces/* proxied to :8081
```

## Production build

```bash
cd admin-ui
npm run build
# → writes ../chassis/server/admin/ui/dist/{index.html, assets/...}
cd ..
go build ./cmd/txco
# → embeds the bundle. Visit http://localhost:8081/admin/ on the next `txco dev`.
```

## Layout

```
src/
├── main.ts              # mount(App, document.getElementById('app'))
├── app.css              # @import "tailwindcss"; plus brand tokens
├── App.svelte           # 2-pane layout
├── lib/
│   ├── api.ts           # fetch wrappers
│   ├── types.ts         # mirrors chassis OpRecord
│   ├── tree.ts          # group ops by stack → scope
│   └── store.svelte.ts  # Svelte 5 runes: ops, selectedId
└── components/
    ├── OpTree.svelte    # sidebar (stack → scope → ops)
    ├── OpDetail.svelte  # tabbed detail view
    ├── JsonPre.svelte   # pretty-printed JSON
    ├── Tabs.svelte      # reusable horizontal tabs
    └── Button.svelte    # reusable button with variants
```

Styling is Tailwind utilities throughout; reusable shapes (`Button`,
`Tabs`) wrap the common compositions so future surfaces (graph view,
trace inspector) don't have to re-derive them.
