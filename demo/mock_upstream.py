#!/usr/bin/env python3
"""Tiny OpenAI-compatible upstream used by demo/quickstart.sh.

It deliberately returns a fixed completion: the demo is about proving the
gateway path (auth -> routing -> quota -> usage), not about calling a real model.
"""

import json
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer


class Handler(BaseHTTPRequestHandler):
    def do_POST(self):
        if not self.path.rstrip("/").endswith("/chat/completions"):
            self.send_error(404)
            return

        length = int(self.headers.get("Content-Length", "0"))
        if length:
            self.rfile.read(length)

        body = {
            "id": "chatcmpl-aigate-demo",
            "object": "chat.completion",
            "created": 0,
            "model": "demo-model",
            "choices": [
                {
                    "index": 0,
                    "message": {
                        "role": "assistant",
                        "content": "Hello from AIGate. The gateway path works.",
                    },
                    "finish_reason": "stop",
                }
            ],
            "usage": {
                "prompt_tokens": 12,
                "completion_tokens": 9,
                "total_tokens": 21,
            },
        }
        data = json.dumps(body).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def log_message(self, fmt, *args):
        print("[mock-upstream] " + fmt % args)


if __name__ == "__main__":
    ThreadingHTTPServer(("0.0.0.0", 18081), Handler).serve_forever()
