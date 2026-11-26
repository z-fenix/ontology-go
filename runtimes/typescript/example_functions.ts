// Example: groundAircraft implemented in TypeScript (same behaviour as the Go and Python versions).
//
//   node --experimental-strip-types runtimes/typescript/example_functions.ts
//
// then set ONTO_FUNCTION_RUNTIMES=typescript=http://localhost:9200 and the action type's function
// to { runtime: typescript, ref: groundAircraft }.

import { randomUUID } from "node:crypto";
import { edit, register, Rejected, serve, type Edit } from "./runtime.ts";

register("groundAircraft", async (ctx) => {
  const ac = ctx.objects.aircraft;
  if (ac.props.status === "GROUNDED") throw new Rejected("ALREADY_GROUNDED", `aircraft ${ac.pk} is already grounded`);
  const reason = String(ctx.params.reason ?? "");
  const edits: Edit[] = [edit.modify(ac, { status: "GROUNDED" })];
  for (const wo of await ctx.find("WorkOrder", { eq: { tailNumber: ac.pk, status: "OPEN" } })) {
    edits.push(edit.modify(wo, { priority: 1 }));
  }
  const id = `WO-${randomUUID()}`;
  edits.push(edit.create("WorkOrder", id, { workOrderId: id, summary: `停场检查：${reason}`, status: "OPEN", priority: 1, tailNumber: ac.pk }));
  return edits;
});

if (process.argv[1]?.endsWith("example_functions.ts")) {
  serve({ port: Number(process.env.PORT ?? 9200) });
}
