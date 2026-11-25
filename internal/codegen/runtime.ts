// ---------------------------------------------------------------------------
// Runtime (dependency-free; uses the global fetch of Node 18+ and browsers)
// ---------------------------------------------------------------------------

/** Primary key of an object, as sent to and returned by the API. */
export type PK = string;

export interface ObjectRef<T extends string = string> {
  type: T;
  pk: PK;
}

/** An object as returned by the query service. Hidden properties are null and listed in __redacted. */
export interface OntologyObject<T extends string, P> {
  type: T;
  pk: PK;
  rid: string;
  version: number;
  properties: P;
  __redacted?: Array<keyof P & string>;
}

export interface Page<O> {
  data: O[];
  nextPageToken?: string;
  ontologyVersion: number;
}

type Scalar = string | number | boolean | null;

/** Object-set filter (one operator per condition). */
export type Where<F extends string = string> =
  | { and: Where<F>[] }
  | { or: Where<F>[] }
  | { not: Where<F> }
  | { eq: { field: F; value: Scalar } }
  | { gt: { field: F; value: Scalar } }
  | { gte: { field: F; value: Scalar } }
  | { lt: { field: F; value: Scalar } }
  | { lte: { field: F; value: Scalar } }
  | { contains: { field: F; value: Scalar } }
  | { startsWith: { field: F; value: string } }
  | { in: { field: F; values: Scalar[] } }
  | { isNull: { field: F } }
  | { link: { path: string; where?: Where<string> } };

export interface OrderBy<F extends string = string> {
  field: F;
  direction?: "asc" | "desc";
}

export interface SearchRequest<F extends string = string> {
  where?: Where<F>;
  orderBy?: OrderBy<F>[];
  pageSize?: number;
  pageToken?: string;
  ontologyVersion?: number;
}

export interface Metric<F extends string = string> {
  op: "count" | "sum" | "avg" | "min" | "max";
  field?: F;
  name?: string;
}

export interface AggregateRequest<F extends string = string> {
  where?: Where<F>;
  groupBy?: F[];
  metrics?: Metric<F>[];
  limit?: number;
  ontologyVersion?: number;
}

export interface AggregateResult {
  groups: Array<{ group?: Record<string, unknown>; metrics: Record<string, unknown> }>;
  truncated: boolean;
  ontologyVersion: number;
}

export interface CriterionResult {
  index: number;
  passed: boolean;
  message?: string;
}

export interface EditSummary {
  created: ObjectRef[];
  modified: ObjectRef[];
  deleted: ObjectRef[];
  links: Array<{ op: string; linkType: string; from: ObjectRef; to: ObjectRef }>;
}

export interface ActionResult {
  actionRid?: string;
  status: "VALID" | "COMMITTED" | "WRITEBACK_PENDING" | string;
  ontologyVersion: number;
  validation: { result: "VALID" | "INVALID"; criteria: CriterionResult[] };
  edits: EditSummary;
  consistencyToken?: string;
}

/** A non-2xx response. errorCode is stable (e.g. SUBMISSION_CRITERIA_FAILED, STALE_OBJECT). */
export class OntologyError extends Error {
  constructor(
    readonly status: number,
    readonly errorCode: string,
    message: string,
    readonly details?: Record<string, unknown>,
  ) {
    super(`${errorCode}: ${message}`);
    this.name = "OntologyError";
  }
}

export interface ClientOptions {
  /** query-service base URL, e.g. https://ontology.example.com */
  queryUrl: string;
  /** action-service base URL; defaults to queryUrl (when both sit behind one gateway). */
  actionUrl?: string;
  /** Returns a bearer token for each request (OIDC access token). */
  token?: () => string | Promise<string>;
  /** Extra headers on every request (e.g. X-Actor for a dev backend). */
  headers?: Record<string, string>;
  /** Custom fetch (tests, proxies). Defaults to globalThis.fetch. */
  fetch?: typeof fetch;
}

export class Transport {
  private readonly f: typeof fetch;
  constructor(private readonly opts: ClientOptions, readonly ontology: string) {
    const f = opts.fetch ?? globalThis.fetch;
    if (!f) throw new Error("no fetch implementation available; pass ClientOptions.fetch");
    this.f = f;
  }

  async request<T>(service: "query" | "action", method: string, path: string, body?: unknown, headers?: Record<string, string>): Promise<T> {
    const base = service === "action" ? (this.opts.actionUrl ?? this.opts.queryUrl) : this.opts.queryUrl;
    const h: Record<string, string> = { Accept: "application/json", ...(this.opts.headers ?? {}), ...(headers ?? {}) };
    if (body !== undefined) h["Content-Type"] = "application/json";
    if (this.opts.token) h["Authorization"] = `Bearer ${await this.opts.token()}`;
    const res = await this.f(base.replace(/\/+$/, "") + `/api/v1/ontologies/${encodeURIComponent(this.ontology)}` + path, {
      method,
      headers: h,
      body: body === undefined ? undefined : JSON.stringify(body),
    });
    const text = await res.text();
    let json: any = undefined;
    if (text) {
      try {
        json = JSON.parse(text);
      } catch {
        json = undefined;
      }
    }
    if (!res.ok) {
      throw new OntologyError(res.status, json?.errorCode ?? "HTTP_" + res.status, json?.message ?? text, json?.details);
    }
    return json as T;
  }
}

/** Typed access to one object type. */
export class ObjectSet<T extends string, P, L extends string> {
  constructor(private readonly t: Transport, readonly type: T) {}

  private path(...parts: string[]): string {
    return "/objects/" + [this.type, ...parts].map(encodeURIComponent).join("/");
  }

  /** One object, or throws OntologyError (404 OBJECT_NOT_FOUND when missing or not visible). */
  get(pk: PK): Promise<OntologyObject<T, P>> {
    return this.t.request("query", "GET", this.path(pk));
  }

  /** One object, or undefined when it is missing or not visible. */
  async find(pk: PK): Promise<OntologyObject<T, P> | undefined> {
    try {
      return await this.get(pk);
    } catch (e) {
      if (e instanceof OntologyError && e.status === 404) return undefined;
      throw e;
    }
  }

  search(req: SearchRequest<keyof P & string> = {}): Promise<Page<OntologyObject<T, P>>> {
    return this.t.request("query", "POST", this.path("search"), req);
  }

  /** Iterates over every matching object, following page tokens. */
  async *all(req: Omit<SearchRequest<keyof P & string>, "pageToken"> = {}): AsyncGenerator<OntologyObject<T, P>> {
    let pageToken: string | undefined;
    do {
      const page = await this.search({ ...req, pageToken });
      for (const o of page.data) yield o;
      pageToken = page.nextPageToken;
    } while (pageToken);
  }

  aggregate(req: AggregateRequest<keyof P & string> = {}): Promise<AggregateResult> {
    return this.t.request("query", "POST", this.path("aggregate"), req);
  }

  /** Objects linked through navigation name `link`. */
  links<R = OntologyObject<string, Record<string, unknown>>>(pk: PK, link: L, pageSize?: number): Promise<Page<R>> {
    const q = pageSize ? `?pageSize=${pageSize}` : "";
    return this.t.request("query", "GET", this.path(pk, "links", link) + q);
  }
}

export interface ApplyOptions {
  /** Makes retries safe: the same key with the same parameters returns the first result. */
  idempotencyKey?: string;
  ontologyVersion?: number;
}

/** Typed access to one action type. */
export class Action<A> {
  constructor(private readonly t: Transport, readonly apiName: string) {}

  private path(op: string): string {
    return `/actions/${encodeURIComponent(this.apiName)}/${op}`;
  }

  /** Checks parameters, permissions and preconditions without writing anything. */
  validate(params: A, opts: ApplyOptions = {}): Promise<ActionResult> {
    return this.t.request("action", "POST", this.path("validate"), { parameters: params, ontologyVersion: opts.ontologyVersion });
  }

  /** Runs the action. Throws OntologyError on rejection (422 criteria, 403 permission, 409 conflict). */
  apply(params: A, opts: ApplyOptions = {}): Promise<ActionResult> {
    const headers = opts.idempotencyKey ? { "Idempotency-Key": opts.idempotencyKey } : undefined;
    return this.t.request("action", "POST", this.path("apply"), { parameters: params, ontologyVersion: opts.ontologyVersion }, headers);
  }

  /** Runs the action for each parameter set; each item commits or fails independently. */
  applyBatch(items: A[]): Promise<{ results: Array<{ index: number; result?: ActionResult; error?: { errorCode: string; message: string } }> }> {
    return this.t.request("action", "POST", this.path("applyBatch"), { requests: items.map((p) => ({ parameters: p })) });
  }
}
