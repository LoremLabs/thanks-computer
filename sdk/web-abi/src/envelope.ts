// The chassis's side of the contract: the request envelope a runner hands
// the bridge, and the delta the bridge answers with.

/** The parts of the chassis's request envelope the bridge reads. */
export interface TxcEnvelope {
  _txc?: {
    client?: { ip?: string };
    web?: {
      req?: {
        method?: string;
        host?: string;
        url?: { full?: string; path?: string; query?: { raw?: string } };
        /** Header name → values, one per line. */
        headers?: Record<string, string[] | string>;
        /** The body, base64; absent when empty. */
        body?: string;
      };
    };
  };
  [k: string]: unknown;
}

/** The response half: what an op writes to answer an HTTP request. */
export interface TxcWebRes {
  status: number;
  /** Lower-case header name → values. */
  headers: Record<string, string[]>;
  /** The body, base64; absent for HEAD, 204 and 304. */
  body?: string;
}

/**
 * The bridge's answer: a delta the chassis merges into the run, never the
 * envelope echoed back (op answers merge by appending arrays, so an echo
 * would duplicate every header).
 */
export interface TxcDelta {
  _txc: { web: { res: TxcWebRes }; halt: true };
}

/** What a server entry gets beside the Request. Deliberately minimal. */
export interface TxcContext {
  readonly client: { readonly ip: string };
}

/** The server/ entry's default export. */
export interface FetchHandler {
  fetch(request: Request, ctx: TxcContext): Response | Promise<Response>;
}
