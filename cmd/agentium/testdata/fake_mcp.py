import json, sys
for line in sys.stdin:
    req = json.loads(line)
    if "id" not in req:
        continue
    m = req["method"]
    if m == "initialize":
        res = {"protocolVersion": "2025-06-18", "capabilities": {"tools": {}}, "serverInfo": {"name": "fake"}}
    elif m == "tools/list":
        res = {"tools": [{"name": "echo", "description": "Echo text", "inputSchema": {"type": "object", "properties": {"text": {"type": "string"}}}}]}
    elif m == "tools/call":
        res = {"content": [{"type": "text", "text": "echoed " + req["params"]["arguments"].get("text", "")}]}
    else:
        print(json.dumps({"jsonrpc": "2.0", "id": req["id"], "error": {"code": -32601, "message": "no"}}), flush=True)
        continue
    print(json.dumps({"jsonrpc": "2.0", "id": req["id"], "result": res}), flush=True)
