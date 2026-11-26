"""Example: groundAircraft implemented in Python (same behaviour as the Go version).

Run:  python3 runtimes/python/example_functions.py
Then point the Action Service at it with ONTO_FUNCTION_RUNTIMES=python=http://localhost:9100 and set
the action type's function to { runtime: python, ref: groundAircraft }.
"""

import os
import uuid

from ontology_runtime import Rejected, edit, function, serve


@function("groundAircraft")
def ground_aircraft(ctx):
    ac = ctx.objects["aircraft"]
    if ac["status"] == "GROUNDED":
        raise Rejected("ALREADY_GROUNDED", "aircraft %s is already grounded" % ac.pk)
    reason = ctx.params.get("reason", "")
    edits = [edit.modify(ac, status="GROUNDED")]
    for wo in ctx.find("WorkOrder", eq={"tailNumber": ac.pk, "status": "OPEN"}):
        edits.append(edit.modify(wo, priority=1))
    wo_id = "WO-" + str(uuid.uuid4())
    edits.append(edit.create("WorkOrder", wo_id, workOrderId=wo_id, summary="停场检查：" + reason,
                             status="OPEN", priority=1, tailNumber=ac.pk))
    return edits


if __name__ == "__main__":
    serve(port=int(os.environ.get("PORT", "9100")))
