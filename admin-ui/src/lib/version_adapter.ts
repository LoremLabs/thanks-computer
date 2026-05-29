// Adapter: VersionDetail → Op[]
//
// The admin UI is op-centric (sidebar tree, box diagram, four tabs)
// but the server's versioned-opstack model is file-centric
// (`stack_files`). One op for the UI = the txcl file under a scope
// + the scope's shared mock-request.json / mock-response.json
// (which apply to every op at that scope).
//
// Path conventions, mirrored from db/schema/sqlite/0011_stacks_versions.sql:
//
//   <scope>/<name>.txcl          one op's resonator source
//   <scope>/mock-request.json    shared per-scope mock input
//   <scope>/mock-response.json   shared per-scope mock output
//
// Anything outside these patterns is currently ignored (with a
// one-shot console warning so a future file convention doesn't fail
// silently).

import type { VersionDetail } from './api'
import type { Op } from './types'

interface ScopeBucket {
    scope: number
    ops: Array<{ name: string; txcl: string; content_hash: string }>
    mockReq?: unknown
    mockRes?: unknown
    mockReqHash?: string
    mockResHash?: string
}

const OP_PATH = /^(\d+)\/([^/]+)\.txcl$/
const MOCK_PATH = /^(\d+)\/mock-(request|response)\.json$/

let warnedUnknownPath = false

export function versionToOps(version: VersionDetail | null | undefined, stackName: string): Op[] {
    if (!version?.files?.length) return []

    const byScope = new Map<number, ScopeBucket>()
    const bucket = (scope: number): ScopeBucket => {
        let b = byScope.get(scope)
        if (!b) {
            b = { scope, ops: [] }
            byScope.set(scope, b)
        }
        return b
    }

    for (const f of version.files) {
        const op = OP_PATH.exec(f.path)
        if (op) {
            const scope = Number(op[1])
            if (!Number.isFinite(scope)) continue
            bucket(scope).ops.push({
                name: op[2],
                txcl: f.content ?? '',
                content_hash: f.content_hash,
            })
            continue
        }
        const mock = MOCK_PATH.exec(f.path)
        if (mock) {
            const scope = Number(mock[1])
            if (!Number.isFinite(scope)) continue
            const parsed = parseJsonSafe(f.content)
            const b = bucket(scope)
            if (mock[2] === 'request') {
                b.mockReq = parsed
                b.mockReqHash = f.content_hash || undefined
            } else {
                b.mockRes = parsed
                b.mockResHash = f.content_hash || undefined
            }
            continue
        }
        if (!warnedUnknownPath) {
            warnedUnknownPath = true
            console.warn(
                `version_adapter: unknown file path "${f.path}" ignored (further warnings suppressed)`
            )
        }
    }

    const out: Op[] = []
    // Sort scopes ascending so the resulting Op[] preserves the same
    // "scope-then-name" ordering OpTree already groups by.
    const scopes = [...byScope.values()].sort((a, b) => a.scope - b.scope)
    for (const b of scopes) {
        b.ops.sort((a, b) => a.name.localeCompare(b.name))
        for (const o of b.ops) {
            out.push({
                stack: stackName,
                scope: b.scope,
                name: o.name,
                txcl: o.txcl,
                mock_req: b.mockReq,
                mock_res: b.mockRes,
                etag: o.content_hash || undefined,
                mock_req_hash: b.mockReqHash,
                mock_res_hash: b.mockResHash,
            })
        }
    }
    return out
}

function parseJsonSafe(s: string | undefined): unknown {
    if (s === undefined || s === '') return undefined
    try {
        return JSON.parse(s)
    } catch {
        return s // surface as a raw string so the user sees what's on disk
    }
}

// fileForOp inverts the mapping for the editor path (Phase 2+). Kept
// alongside versionToOps so the path conventions live in one file.
// Unused for now; exporting so future code has it.
export function fileForOp(scope: number, name: string): string {
    return `${scope}/${name}.txcl`
}

export function mockReqPath(scope: number): string {
    return `${scope}/mock-request.json`
}

export function mockResPath(scope: number): string {
    return `${scope}/mock-response.json`
}
