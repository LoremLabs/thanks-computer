// Mirrors chassis/server/admin/ops.go:OpRecord. Keep field names in
// lockstep with the Go json tags.
export interface Op {
    stack: string
    scope: number
    name: string
    txcl: string
    mock_req?: unknown
    mock_res?: unknown
    revision?: number
    // content_hash of the .txcl file in the current version; passed
    // as base_hash on PATCH for optimistic concurrency.
    etag?: string
    // Per-scope mock-file hashes — sibling to etag but for the
    // mock-request.json / mock-response.json files. Populated by the
    // version_adapter from VersionDetail.files. May be undefined when
    // the scope has no mock file (the scope's mock fields are
    // shared across all ops at that scope; same hash everywhere).
    mock_req_hash?: string
    mock_res_hash?: string
}

export interface ListOpsResponse {
    ops: Op[]
}

export function opId(op: Op): string {
    return `${op.stack}/${op.scope}/${op.name}`
}
