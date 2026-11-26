"""Python function runtime for ontology-go actions (standard library only, Python 3.9+).

Register functions with @function("<ref>") and call serve(). The Action Service POSTs each
invocation to /functions/<ref>; the function returns a list of edits and may read further objects
through ctx.get / ctx.find, which call back into the Action Service with the caller's permissions.

    from ontology_runtime import function, edit, serve, Rejected

    @function("groundAircraft")
    def ground_aircraft(ctx):
        ac = ctx.objects["aircraft"]
        ...
        return [edit.modify(ac, status="GROUNDED")]

    serve(port=9100)

Raise Rejected(code, message) for a business-rule failure (HTTP 422, shown to the caller as is);
any other exception is reported as FUNCTION_FAILED.
"""

import json
import os
import urllib.error
import urllib.request
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

_FUNCTIONS = {}
MAX_REQUEST = 8 << 20


class Rejected(Exception):
    """A business-rule rejection, returned to the caller with its code."""

    def __init__(self, code, message):
        super().__init__(message)
        self.code = code
        self.message = message


class ReadError(Exception):
    """A read callback failed (object missing or not visible, read limit exceeded, ...)."""


class Obj:
    """An ontology object as passed to a function."""

    def __init__(self, d):
        self.type = d["type"]
        self.pk = d["pk"]
        self.version = d.get("version", 0)
        self.props = d.get("props") or {}

    def __getitem__(self, name):
        return self.props.get(name)

    def __repr__(self):
        return "Obj(%s:%s v%d)" % (self.type, self.pk, self.version)


class Context:
    def __init__(self, req, timeout=10):
        self.ref = req["ref"]
        self.actor = req.get("actor") or {}
        self.params = req.get("params") or {}
        self.objects = {k: Obj(v) for k, v in (req.get("objects") or {}).items() if v}
        self.lists = {k: [Obj(o) for o in v or []] for k, v in (req.get("lists") or {}).items()}
        self._reader = (req.get("reader") or {}).get("url", "")
        self._timeout = timeout

    def _call(self, op, body):
        data = json.dumps(body).encode()
        r = urllib.request.Request(self._reader + "/" + op, data=data, method="POST",
                                   headers={"Content-Type": "application/json"})
        try:
            with urllib.request.urlopen(r, timeout=self._timeout) as resp:
                return json.loads(resp.read())
        except urllib.error.HTTPError as e:
            try:
                msg = json.loads(e.read()).get("message", "")
            except Exception:
                msg = ""
            raise ReadError("%s failed (HTTP %d): %s" % (op, e.code, msg)) from None

    def get(self, type_, pk):
        """Reads one object; raises ReadError if it is missing or not visible to the caller."""
        return Obj(self._call("get", {"type": type_, "pk": pk}))

    def find(self, type_, eq=None, in_=None, limit=1000):
        """Finds objects by equality / membership filters (only visible objects are returned)."""
        body = {"type": type_, "limit": limit}
        if eq:
            body["eq"] = eq
        if in_:
            body["in"] = in_
        return [Obj(o) for o in self._call("find", body).get("data", [])]


def _ref(o):
    return (o.type, o.pk, o.version) if isinstance(o, Obj) else (o[0], o[1], 0)


class edit:
    """Edit constructors. Objects may be Obj instances or (type, pk) tuples."""

    @staticmethod
    def create(type_, pk, **changes):
        return {"op": "CREATE", "type": type_, "pk": pk, "changes": changes}

    @staticmethod
    def modify(obj, **changes):
        t, pk, v = _ref(obj)
        e = {"op": "MODIFY", "type": t, "pk": pk, "changes": changes}
        if v:
            e["expectedVersion"] = v  # optimistic locking against the version the function saw
        return e

    @staticmethod
    def delete(obj):
        t, pk, v = _ref(obj)
        e = {"op": "DELETE", "type": t, "pk": pk}
        if v:
            e["expectedVersion"] = v
        return e

    @staticmethod
    def link(obj, link_type, target):
        t, pk, _ = _ref(obj)
        tt, tpk, _ = _ref(target)
        return {"op": "LINK", "type": t, "pk": pk, "linkType": link_type, "target": {"type": tt, "pk": tpk}}

    @staticmethod
    def unlink(obj, link_type, target):
        e = edit.link(obj, link_type, target)
        e["op"] = "UNLINK"
        return e


def function(ref):
    """Registers fn(ctx) -> list of edits under ref."""

    def deco(fn):
        _FUNCTIONS[ref] = fn
        return fn

    return deco


def invoke(req):
    """Runs one invocation request and returns (status, body). Exposed for tests."""
    fn = _FUNCTIONS.get(req.get("ref", ""))
    if fn is None:
        return 404, {"errorCode": "FUNCTION_NOT_REGISTERED", "message": "function %r is not registered" % req.get("ref")}
    try:
        edits = fn(Context(req))
    except Rejected as r:
        return 422, {"errorCode": r.code, "message": r.message}
    except Exception as e:  # noqa: BLE001 - reported to the Action Service as FUNCTION_FAILED
        return 500, {"message": "%s: %s" % (type(e).__name__, e)}
    return 200, {"edits": list(edits or [])}


class _Handler(BaseHTTPRequestHandler):
    token = ""

    def _send(self, status, body):
        raw = json.dumps(body, default=str).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(raw)))
        self.end_headers()
        self.wfile.write(raw)

    def do_GET(self):
        if self.path == "/healthz":
            self._send(200, {"functions": sorted(_FUNCTIONS)})
        else:
            self._send(404, {"message": "not found"})

    def do_POST(self):
        if self.token and self.headers.get("Authorization") != "Bearer " + self.token:
            self._send(401, {"message": "unauthorized"})
            return
        if not self.path.startswith("/functions/"):
            self._send(404, {"message": "not found"})
            return
        n = int(self.headers.get("Content-Length") or 0)
        if n > MAX_REQUEST:
            self._send(413, {"message": "request too large"})
            return
        try:
            req = json.loads(self.rfile.read(n))
        except ValueError:
            self._send(400, {"message": "invalid JSON"})
            return
        req["ref"] = urllib.request.unquote(self.path[len("/functions/"):])
        self._send(*invoke(req))

    def log_message(self, fmt, *args):  # quieter than the default stderr access log
        if os.environ.get("ONTO_RUNTIME_ACCESS_LOG"):
            super().log_message(fmt, *args)


def serve(host="0.0.0.0", port=9100, token=None):
    """Serves registered functions. token defaults to ONTO_FUNCTION_RUNTIME_TOKEN."""
    _Handler.token = token if token is not None else os.environ.get("ONTO_FUNCTION_RUNTIME_TOKEN", "")
    srv = ThreadingHTTPServer((host, port), _Handler)
    print("python function runtime on %s:%d: %s" % (host, port, ", ".join(sorted(_FUNCTIONS)) or "(none)"))
    srv.serve_forever()
