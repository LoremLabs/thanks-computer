// Deterministic name → palette color, ported from
// loremlabs/inamoon/extension/src/lib/colorizer.ts. The palette is
// pre-resolved (the source iterates tailwindcss v3's color palette at
// runtime; we bake the resulting hex values here so admin-ui keeps a
// zero-runtime-dep version that yields byte-identical output for the
// same input). djb2 string-hash inlined for the same reason — the
// upstream module is ~10 lines.

export const defaultPalette: string[] = [
    '#3366CC', '#FF9900', '#109618', '#990099', '#3B3EAC',
    '#0099C6', '#DD4477', '#66AA00', '#FF0099', '#B82E2E',
    '#316395', '#994499', '#22AA99', '#AACA51', '#DC3912',
    '#6633CC', '#F67370', '#8B0707', '#329262', '#5574A6',
    '#3B3EAC',
    // tailwind v3 shades 400/600/800 for red, blue, green, yellow,
    // pink, amber, purple, sky, indigo, rose, slate — in that order.
    '#f87171', '#dc2626', '#991b1b',
    '#60a5fa', '#2563eb', '#1e40af',
    '#4ade80', '#16a34a', '#166534',
    '#facc15', '#ca8a04', '#854d0e',
    '#f472b6', '#db2777', '#9d174d',
    '#fbbf24', '#d97706', '#92400e',
    '#c084fc', '#9333ea', '#6b21a8',
    '#38bdf8', '#0284c7', '#075985',
    '#818cf8', '#4f46e5', '#3730a3',
    '#fb7185', '#e11d48', '#9f1239',
    '#94a3b8', '#475569', '#1e293b',
]

function stringHash(s: string): number {
    let hash = 5381
    let i = s.length
    while (i) {
        hash = (hash * 33) ^ s.charCodeAt(--i)
    }
    return hash >>> 0
}

export function colorizer(name: string, palette: string[] = defaultPalette): string {
    name = (name || '??').toLowerCase()
    return palette[stringHash(name) % palette.length]
}
