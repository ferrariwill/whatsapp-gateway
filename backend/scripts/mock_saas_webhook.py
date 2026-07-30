"""Mock SaaS webhook receiver for integration tests."""
import json
import sys
from http.server import BaseHTTPRequestHandler, HTTPServer

PORT = int(sys.argv[1]) if len(sys.argv) > 1 else 9999
OUTPUT = sys.argv[2] if len(sys.argv) > 2 else "saas_webhook_received.json"
received = []


class Handler(BaseHTTPRequestHandler):
    def log_message(self, fmt, *args):
        print(f"[mock-saas] {fmt % args}")

    def do_POST(self):
        length = int(self.headers.get("Content-Length", 0))
        body = self.rfile.read(length).decode("utf-8")
        payload = json.loads(body) if body else {}
        received.append(payload)
        with open(OUTPUT, "w", encoding="utf-8") as f:
            json.dump(received, f, indent=2, ensure_ascii=False)
        print(f"[mock-saas] POST {self.path} -> {payload}")
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.end_headers()
        self.wfile.write(b'{"ok":true}')


if __name__ == "__main__":
    print(f"[mock-saas] listening on :{PORT}, writing to {OUTPUT}")
    HTTPServer(("127.0.0.1", PORT), Handler).serve_forever()
