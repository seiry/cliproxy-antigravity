#!/usr/bin/env python3
"""End-to-end check of acp mode against a running CLIProxyAPI that loads this plugin.

Run on the Docker host of the CLIProxyAPI container:

    python3 scripts/e2e_acp.py --base http://127.0.0.1:18317 --key-file /path/api-key \
        --container cpa-acp-staging --report e2e-report.json

It sends about ten real prompts, so it costs a little Antigravity quota. The report records
every step's timing and verdict; the exit code is non-zero when any step fails.
"""
import argparse
import base64
import json
import struct
import subprocess
import sys
import threading
import time
import urllib.request
import zlib

MODEL = "agy/gemini-4-hydrogen-low"


def residents(container):
    """Resident agy acp processes as {pid: HOME}."""
    out = subprocess.run(
        ["docker", "exec", container, "sh", "-c",
         'for p in /proc/[0-9]*; do c=$(tr "\\0" " " < $p/cmdline 2>/dev/null); '
         'case "$c" in */agy\\ acp*) echo ${p#/proc/} $(tr "\\0" "\\n" < $p/environ | grep ^HOME=);; esac; done'],
        capture_output=True, text=True, check=True).stdout
    found = {}
    for line in out.splitlines():
        pid, _, home = line.partition(" ")
        found[int(pid)] = home.removeprefix("HOME=")
    return found


def agy_pids(container):
    return sorted(residents(container))


def one_per_home(container):
    homes = list(residents(container).values())
    return len(homes) == len(set(homes))


def container_hostname(container):
    return subprocess.run(["docker", "exec", container, "cat", "/etc/hostname"],
                          capture_output=True, text=True, check=True).stdout.strip()


def red_png(size=32):
    raw = b"".join(b"\x00" + b"\xff\x00\x00" * size for _ in range(size))

    def chunk(kind, data):
        return struct.pack(">I", len(data)) + kind + data + struct.pack(">I", zlib.crc32(kind + data) & 0xFFFFFFFF)

    png = b"\x89PNG\r\n\x1a\n" + chunk(b"IHDR", struct.pack(">IIBBBBB", size, size, 8, 2, 0, 0, 0))
    png += chunk(b"IDAT", zlib.compress(raw)) + chunk(b"IEND", b"")
    return "data:image/png;base64," + base64.b64encode(png).decode()


class Client:
    def __init__(self, base, key):
        self.base, self.key = base.rstrip("/"), key

    def _req(self, body):
        return urllib.request.Request(
            self.base + "/v1/chat/completions", data=json.dumps(body).encode(),
            headers={"Authorization": "Bearer " + self.key, "Content-Type": "application/json"})

    def chat(self, messages, **extra):
        t0 = time.time()
        with urllib.request.urlopen(self._req({"model": MODEL, "messages": messages, **extra}), timeout=180) as r:
            body = json.load(r)
        return body, time.time() - t0

    def stream(self, messages, **extra):
        t0 = time.time()
        chunks, first, text = 0, None, []
        body = {"model": MODEL, "messages": messages, "stream": True, "stream_options": {"include_usage": True}, **extra}
        usage = None
        with urllib.request.urlopen(self._req(body), timeout=180) as r:
            for raw in r:
                line = raw.decode().strip()
                if not line.startswith("data:") or line == "data: [DONE]":
                    continue
                d = json.loads(line[5:])
                usage = d.get("usage") or usage
                for c in d.get("choices", []):
                    piece = (c.get("delta") or {}).get("content")
                    if piece:
                        chunks += 1
                        first = first or time.time() - t0
                        text.append(piece)
        return {"text": "".join(text), "chunks": chunks, "first_chunk_s": first,
                "total_s": time.time() - t0, "usage": usage}


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--base", required=True)
    ap.add_argument("--key-file", required=True)
    ap.add_argument("--container", required=True)
    ap.add_argument("--report", default="e2e-report.json")
    a = ap.parse_args()
    cli = Client(a.base, open(a.key_file).read().strip())
    report = {"model": MODEL, "started": time.strftime("%Y-%m-%dT%H:%M:%S%z"), "steps": []}

    def step(name, fn):
        t0 = time.time()
        try:
            ok, detail = fn()
        except Exception as e:  # noqa: BLE001 - a failed step is a verdict, not a crash
            ok, detail = False, {"exception": repr(e)}
        report["steps"].append({"name": name, "ok": ok, "seconds": round(time.time() - t0, 2), **detail})
        print(("PASS " if ok else "FAIL ") + name, json.dumps(detail)[:300], flush=True)

    # Warm the resident process so later timings exclude the one-off start.
    step("warmup", lambda: (lambda b: (b[0]["choices"][0]["message"]["content"].strip() != "", {"latency_s": round(b[1], 2)}))(
        cli.chat([{"role": "user", "content": "Reply with just: ready"}])))
    baseline = agy_pids(a.container)
    report["resident_pids_at_start"] = baseline

    def concurrent_streams():
        prompts = ["List the numbers 1 to 30 separated by commas, nothing else.",
                   "Name the seven days of the week separated by commas, nothing else.",
                   "Write the alphabet letters A to Z separated by spaces, nothing else."]
        results = [None] * len(prompts)

        def run(i):
            results[i] = cli.stream([{"role": "user", "content": prompts[i]}])
        threads = [threading.Thread(target=run, args=(i,)) for i in range(len(prompts))]
        t0 = time.time()
        [t.start() for t in threads]
        [t.join() for t in threads]
        wall = time.time() - t0
        checks = ["30" in results[0]["text"], "Sunday" in results[1]["text"], "Z" in results[2]["text"]]
        streamed = all(r["chunks"] >= 1 and r["usage"] for r in results)
        slowest = max(r["total_s"] for r in results)
        pids = agy_pids(a.container)
        return (all(checks) and streamed and one_per_home(a.container) and set(baseline) <= set(pids),
                {"wall_s": round(wall, 2), "serial_sum_s": round(sum(r["total_s"] for r in results), 2),
                 "slowest_s": round(slowest, 2), "pids_after": pids,
                 "per_stream": [{k: (round(v, 2) if isinstance(v, float) else v) for k, v in r.items() if k != "text"} for r in results],
                 "content_ok": checks})
    step("three concurrent streams on one resident process", concurrent_streams)

    def tool_round_trip():
        tools = [{"type": "function", "function": {
            "name": "get_weather", "description": "Current weather for a city",
            "parameters": {"type": "object", "properties": {"city": {"type": "string"}}, "required": ["city"]}}}]
        msgs = [{"role": "user", "content": "What is the weather in Oslo right now? Use the tool."}]
        b1, l1 = cli.chat(msgs, tools=tools)
        choice = b1["choices"][0]
        calls = choice["message"].get("tool_calls") or []
        if not calls or calls[0]["function"]["name"] != "get_weather":
            return False, {"first_turn": choice}
        msgs += [choice["message"], {"role": "tool", "tool_call_id": calls[0]["id"],
                                     "content": json.dumps({"city": "Oslo", "temp_c": -7, "sky": "snow"})}]
        b2, l2 = cli.chat(msgs, tools=tools)
        answer = b2["choices"][0]["message"].get("content") or ""
        return ("-7" in answer or "snow" in answer.lower(),
                {"finish_reason_1": choice.get("finish_reason"), "args": calls[0]["function"]["arguments"],
                 "answer": answer[:160], "latency_s": [round(l1, 2), round(l2, 2)]})
    step("client tool call round trip", tool_round_trip)

    def denied_builtin_tool():
        secret = container_hostname(a.container)
        b, lat = cli.chat([{"role": "user", "content":
                            "Use your file reading tool to read /etc/hostname and print its exact contents."}])
        answer = b["choices"][0]["message"].get("content") or ""
        return secret not in answer, {"leaked": secret in answer, "answer": answer[:160], "latency_s": round(lat, 2)}
    step("built-in file tool is denied", denied_builtin_tool)

    def image_block():
        b, lat = cli.chat([{"role": "user", "content": [
            {"type": "text", "text": "What single colour fills this image? Answer with one word."},
            {"type": "image_url", "image_url": {"url": red_png()}}]}])
        answer = (b["choices"][0]["message"].get("content") or "").strip()
        return "red" in answer.lower(), {"answer": answer[:80], "latency_s": round(lat, 2)}
    step("image sent as an ACP block", image_block)

    def crash_recovery():
        before = agy_pids(a.container)
        subprocess.run(["docker", "exec", a.container, "sh", "-c", "kill -9 " + " ".join(map(str, before))], check=True)
        time.sleep(1)
        b, lat = cli.chat([{"role": "user", "content": "Reply with just: back"}])
        answer = b["choices"][0]["message"].get("content") or ""
        after = agy_pids(a.container)
        return ("back" in answer.lower() and len(after) >= 1 and set(after).isdisjoint(before),
                {"killed": before, "restarted": after, "answer": answer[:40], "latency_s": round(lat, 2)})
    step("resident agy killed, next request restarts it", crash_recovery)

    def steady_state():
        times = [round(cli.chat([{"role": "user", "content": f"What is {n}*7? Reply with just the number."}])[1], 2)
                 for n in (6, 9)]
        return True, {"latencies_s": times}
    step("warm latency", steady_state)

    report["residents_at_end"] = residents(a.container)
    report["one_resident_per_home"] = one_per_home(a.container)
    report["passed"] = all(s["ok"] for s in report["steps"]) and report["one_resident_per_home"]
    with open(a.report, "w") as f:
        json.dump(report, f, indent=2)
    print("report:", a.report, "passed:", report["passed"])
    sys.exit(0 if report["passed"] else 1)


if __name__ == "__main__":
    main()
