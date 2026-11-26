// node --experimental-strip-types --test runtimes/typescript/runtime.test.ts

import { test, before, after } from "node:test";
import assert from "node:assert/strict";
import { createServer, type Server } from "node:http";
import type { AddressInfo } from "node:net";
import { invoke, type InvocationRequest } from "./runtime.ts";
import "./example_functions.ts";

let reader: Server;
let readerURL = "";

before(async () => {
  // Plays the Action Service's read callback.
  reader = createServer((req, res) => {
    let raw = "";
    req.on("data", (c) => (raw += c));
    req.on("end", () => {
      const body = JSON.parse(raw);
      const ok = req.url?.endsWith("/find") && body.type === "WorkOrder";
      const out = ok ? { data: [{ type: "WorkOrder", pk: "WO-1", version: 4, props: { status: "OPEN" } }] } : { message: "not visible" };
      res.writeHead(ok ? 200 : 404, { "Content-Type": "application/json" });
      res.end(JSON.stringify(out));
    });
  });
  await new Promise<void>((r) => reader.listen(0, "127.0.0.1", () => r()));
  readerURL = `http://127.0.0.1:${(reader.address() as AddressInfo).port}/internal/function-reads/tok`;
});

after(() => reader.close());

function request(status = "ACTIVE"): InvocationRequest {
  return {
    ref: "groundAircraft",
    actor: { id: "alice", kind: "USER" },
    params: { reason: "bird strike" },
    objects: { aircraft: { type: "Aircraft", pk: "B-1234", version: 2, props: { status } } },
    lists: {},
    reader: { url: readerURL },
  };
}

test("groundAircraft returns edits", async () => {
  const [status, body] = (await invoke(request())) as [number, { edits: any[] }];
  assert.equal(status, 200);
  assert.deepEqual(body.edits[0], { op: "MODIFY", type: "Aircraft", pk: "B-1234", changes: { status: "GROUNDED" }, expectedVersion: 2 });
  assert.equal(body.edits[1].pk, "WO-1");
  assert.equal(body.edits[1].expectedVersion, 4);
  assert.equal(body.edits[2].op, "CREATE");
  assert.equal(body.edits[2].changes.tailNumber, "B-1234");
});

test("business rule rejection is a 422 with its code", async () => {
  const [status, body] = (await invoke(request("GROUNDED"))) as [number, any];
  assert.equal(status, 422);
  assert.equal(body.errorCode, "ALREADY_GROUNDED");
});

test("unknown function is a 404", async () => {
  const [status] = await invoke({ ...request(), ref: "nope" });
  assert.equal(status, 404);
});

test("a failing read is reported as a failure", async () => {
  const [status, body] = (await invoke({ ...request(), reader: { url: "http://127.0.0.1:1/x" } })) as [number, any];
  assert.equal(status, 500);
  assert.ok(body.message);
});
