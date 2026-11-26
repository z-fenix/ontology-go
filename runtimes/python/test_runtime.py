"""Tests for the Python runtime: python3 runtimes/python/test_runtime.py"""

import json
import os
import sys
import threading
import unittest
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

import example_functions  # noqa: E402,F401  (registers groundAircraft)
from ontology_runtime import invoke  # noqa: E402


class FakeReader(BaseHTTPRequestHandler):
    """Plays the Action Service's read callback."""

    def do_POST(self):
        body = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
        if self.path.endswith("/find") and body["type"] == "WorkOrder":
            out = {"data": [{"type": "WorkOrder", "pk": "WO-1", "version": 4, "props": {"status": "OPEN"}}]}
            status = 200
        else:
            out, status = {"message": "not visible"}, 404
        raw = json.dumps(out).encode()
        self.send_response(status)
        self.send_header("Content-Length", str(len(raw)))
        self.end_headers()
        self.wfile.write(raw)

    def log_message(self, *args):
        pass


class RuntimeTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.srv = ThreadingHTTPServer(("127.0.0.1", 0), FakeReader)
        threading.Thread(target=cls.srv.serve_forever, daemon=True).start()
        cls.reader = "http://127.0.0.1:%d/internal/function-reads/tok" % cls.srv.server_address[1]

    @classmethod
    def tearDownClass(cls):
        cls.srv.shutdown()

    def request(self, status="ACTIVE"):
        return {"ref": "groundAircraft", "actor": {"id": "alice", "kind": "USER"},
                "params": {"reason": "bird strike"},
                "objects": {"aircraft": {"type": "Aircraft", "pk": "B-1234", "version": 2, "props": {"status": status}}},
                "reader": {"url": self.reader}}

    def test_ground_aircraft(self):
        status, body = invoke(self.request())
        self.assertEqual(status, 200, body)
        edits = body["edits"]
        self.assertEqual(edits[0], {"op": "MODIFY", "type": "Aircraft", "pk": "B-1234",
                                    "changes": {"status": "GROUNDED"}, "expectedVersion": 2})
        self.assertEqual(edits[1]["pk"], "WO-1")
        self.assertEqual(edits[1]["expectedVersion"], 4)
        self.assertEqual(edits[2]["op"], "CREATE")
        self.assertEqual(edits[2]["changes"]["tailNumber"], "B-1234")
        json.dumps(body)  # the response must be serialisable

    def test_rejection(self):
        status, body = invoke(self.request(status="GROUNDED"))
        self.assertEqual((status, body["errorCode"]), (422, "ALREADY_GROUNDED"))

    def test_unknown_function(self):
        status, _ = invoke({"ref": "nope"})
        self.assertEqual(status, 404)

    def test_failure(self):
        req = self.request()
        req["reader"]["url"] = "http://127.0.0.1:1/unreachable"
        status, body = invoke(req)
        self.assertEqual(status, 500)
        self.assertIn("message", body)


if __name__ == "__main__":
    unittest.main()
