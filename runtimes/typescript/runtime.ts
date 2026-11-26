// TypeScript function runtime for ontology-go actions. No dependencies; runs on Node 22.6+ with
// type stripping (node --experimental-strip-types), or after compiling with tsc.
//
//   import { register, serve, edit, Rejected } from "./runtime.ts";
//   register("groundAircraft", async (ctx) => [edit.modify(ctx.objects.aircraft, { status: "GROUNDED" })]);
//   serve({ port: 9200 });
//
// The Action Service POSTs each invocation to /functions/<ref>. A function returns edits and may
// read more objects with ctx.get / ctx.find, which call back into the Action Service with the
// caller's permissions. Throw Rejected for a business-rule failure (HTTP 422); any other error is
// reported as FUNCTION_FAILED.

import { createServer, type IncomingMessage, type ServerResponse, type Server } from "node:http";

export interface Obj {
  type: string;
  pk: string;
  version: number;
  props: Record<string, unknown>;
}

export interface Edit {
  op: "CREATE" | "MODIFY" | "DELETE" | "LINK" | "UNLINK";
  type: string;
  pk: string;
  changes?: Record<string, unknown>;
  expectedVersion?: number;
  linkType?: string;
  target?: { type: string; pk: string };
}

export interface InvocationRequest {
  ref: string;
  actor: { id: string; kind: string; onBehalfOf?: string };
  params: Record<string, unknown>;
  objects: Record<string, Obj | null>;
  lists: Record<string, Obj[]>;
  reader: { url: string };
}

export class Rejected extends Error {
  code: string;
  constructor(code: string, message: string) {
    super(message);
    this.code = code;
  }
}

export class ReadError extends Error {}

export class Context {
  ref: string;
  actor: InvocationRequest["actor"];
  params: Record<string, unknown>;
  objects: Record<string, Obj>;
  lists: Record<string, Obj[]>;
  private readerURL: string;

  constructor(req: InvocationRequest) {
    this.ref = req.ref;
    this.actor = req.actor ?? { id: "", kind: "" };
    this.params = req.params ?? {};
    this.objects = {};
    for (const [k, v] of Object.entries(req.objects ?? {})) {
      if (v) this.objects[k] = v;
    }
    this.lists = req.lists ?? {};
    this.readerURL = req.reader?.url ?? "";
  }

  private async call(op: string, body: unknown): Promise<any> {
    const resp = await fetch(`${this.readerURL}/${op}`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(body),
      signal: AbortSignal.timeout(10_000),
    });
    const data: any = await resp.json().catch(() => ({}));
    if (!resp.ok) throw new ReadError(`${op} failed (HTTP ${resp.status}): ${data.message ?? ""}`);
    return data;
  }

  /** Reads one object; throws ReadError if it is missing or not visible to the caller. */
  get(type: string, pk: string): Promise<Obj> {
    return this.call("get", { type, pk });
  }

  /** Finds objects by equality / membership filters (only visible objects are returned). */
  async find(type: string, opts: { eq?: Record<string, unknown>; in?: Record<string, unknown[]>; limit?: number } = {}): Promise<Obj[]> {
    const out = await this.call("find", { type, eq: opts.eq, in: opts.in, limit: opts.limit ?? 1000 });
    return out.data ?? [];
  }
}

type Ref = Obj | { type: string; pk: string; version?: number };

function versioned(o: Ref): { expectedVersion?: number } {
  return o.version ? { expectedVersion: o.version } : {};
}

export const edit = {
  create(type: string, pk: string, changes: Record<string, unknown>): Edit {
    return { op: "CREATE", type, pk, changes };
  },
  modify(o: Ref, changes: Record<string, unknown>): Edit {
    return { op: "MODIFY", type: o.type, pk: o.pk, changes, ...versioned(o) };
  },
  delete(o: Ref): Edit {
    return { op: "DELETE", type: o.type, pk: o.pk, ...versioned(o) };
  },
  link(o: Ref, linkType: string, target: Ref): Edit {
    return { op: "LINK", type: o.type, pk: o.pk, linkType, target: { type: target.type, pk: target.pk } };
  },
  unlink(o: Ref, linkType: string, target: Ref): Edit {
    return { op: "UNLINK", type: o.type, pk: o.pk, linkType, target: { type: target.type, pk: target.pk } };
  },
};

export type Fn = (ctx: Context) => Edit[] | Promise<Edit[]>;

const functions = new Map<string, Fn>();

export function register(ref: string, fn: Fn): void {
  functions.set(ref, fn);
}

/** Runs one invocation and returns [status, body]. Exposed for tests. */
export async function invoke(req: InvocationRequest): Promise<[number, unknown]> {
  const fn = functions.get(req.ref);
  if (!fn) return [404, { errorCode: "FUNCTION_NOT_REGISTERED", message: `function "${req.ref}" is not registered` }];
  try {
    return [200, { edits: (await fn(new Context(req))) ?? [] }];
  } catch (e) {
    if (e instanceof Rejected) return [422, { errorCode: e.code, message: e.message }];
    const err = e as Error;
    return [500, { message: `${err?.name ?? "Error"}: ${err?.message ?? String(e)}` }];
  }
}

const MAX_REQUEST = 8 << 20;

function send(res: ServerResponse, status: number, body: unknown): void {
  const raw = JSON.stringify(body);
  res.writeHead(status, { "Content-Type": "application/json", "Content-Length": Buffer.byteLength(raw) });
  res.end(raw);
}

async function readBody(req: IncomingMessage): Promise<string> {
  let size = 0;
  const chunks: Buffer[] = [];
  for await (const c of req) {
    size += (c as Buffer).length;
    if (size > MAX_REQUEST) throw new Error("request too large");
    chunks.push(c as Buffer);
  }
  return Buffer.concat(chunks).toString("utf8");
}

/** Serves registered functions. token defaults to ONTO_FUNCTION_RUNTIME_TOKEN. */
export function serve(opts: { port?: number; host?: string; token?: string } = {}): Server {
  const token = opts.token ?? process.env.ONTO_FUNCTION_RUNTIME_TOKEN ?? "";
  const server = createServer(async (req, res) => {
    if (req.method === "GET" && req.url === "/healthz") return send(res, 200, { functions: [...functions.keys()].sort() });
    if (req.method !== "POST" || !req.url?.startsWith("/functions/")) return send(res, 404, { message: "not found" });
    if (token && req.headers.authorization !== `Bearer ${token}`) return send(res, 401, { message: "unauthorized" });
    let body: InvocationRequest;
    try {
      body = JSON.parse(await readBody(req));
    } catch {
      return send(res, 400, { message: "invalid request body" });
    }
    body.ref = decodeURIComponent(req.url.slice("/functions/".length));
    const [status, out] = await invoke(body);
    send(res, status, out);
  });
  server.listen(opts.port ?? 9200, opts.host ?? "0.0.0.0", () => {
    console.log(`typescript function runtime on :${opts.port ?? 9200}: ${[...functions.keys()].join(", ") || "(none)"}`);
  });
  return server;
}
