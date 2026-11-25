// Type-checked in CI against the SDK generated from examples/ontology (see ci.yml).
// It is also a usage example.
import { createClient, OntologyError, type WorkOrder } from "./mro";

async function main(): Promise<void> {
  const client = createClient({
    queryUrl: "http://localhost:8082",
    actionUrl: "http://localhost:8081",
    headers: { "X-Actor": "alice" }, // dev backend; use `token: () => accessToken` in production
  });

  // Typed filters: field names are checked against WorkOrderProperties.
  const page = await client.objects.WorkOrder.search({
    where: {
      and: [
        { eq: { field: "status", value: "OPEN" } },
        { link: { path: "aircraft", where: { eq: { field: "baseAirport", value: "SIN" } } } },
      ],
    },
    orderBy: [{ field: "priority", direction: "desc" }],
    pageSize: 20,
  });

  const first: WorkOrder | undefined = page.data[0];
  if (first) {
    const status: "OPEN" | "IN_PROGRESS" | "DONE" | "CANCELLED" | null = first.properties.status;
    const cost: string | null = first.properties.estimatedCost; // null when redacted
    console.log(first.pk, status, cost, first.__redacted ?? []);
    const aircraft = await client.objects.WorkOrder.links(first.pk, "aircraft");
    console.log(aircraft.data.length);
  }

  for await (const wo of client.objects.WorkOrder.all({ where: { isNull: { field: "technicianId" } } })) {
    console.log(wo.pk);
  }

  const stats = await client.objects.WorkOrder.aggregate({ groupBy: ["tailNumber"], metrics: [{ op: "count" }] });
  console.log(stats.groups.length);

  try {
    const res = await client.actions.assignWorkOrder.apply(
      { workOrder: "WO-1", technician: "T-1", note: "night shift" },
      { idempotencyKey: "assign-WO-1-T-1" },
    );
    console.log(res.status, res.edits.modified);
  } catch (e) {
    if (e instanceof OntologyError && e.errorCode === "SUBMISSION_CRITERIA_FAILED") {
      console.log("precondition failed:", e.message);
    } else {
      throw e;
    }
  }
}

void main;

// @ts-expect-error unknown property name must not type-check
void (() => createClient({ queryUrl: "" }).objects.WorkOrder.search({ where: { eq: { field: "nope", value: 1 } } }));
// @ts-expect-error required action parameter "technician" is missing
void (() => createClient({ queryUrl: "" }).actions.assignWorkOrder.apply({ workOrder: "WO-1" }));
// @ts-expect-error unknown link name
void (() => createClient({ queryUrl: "" }).objects.Aircraft.links("B-1", "engines"));
