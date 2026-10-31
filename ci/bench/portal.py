#!/usr/bin/env python3
"""ISUCON ポータルのベンチマーク API クライアント（標準ライブラリのみ）。

ポータル（isuxportal）は protobuf を返す。ブラウザの Enqueue ボタンと同じリクエストを送る:
  POST /api/contestant/benchmark_jobs  body=EnqueueBenchmarkJobRequest{target_id=1}
  ヘッダ: Content-Type: application/vnd.google.protobuf, X-CSRF-Token（HTML の meta csrf-token）

  portal.py active               # PENDING/RUNNING のジョブ id を表示（無ければ何も出さない）
  portal.py latest-id            # 最新ジョブの id
  portal.py list [N]             # 最新 N 件を JSON lines で
  portal.py enqueue              # ジョブを投げて id を表示
  portal.py wait ID              # 終わるまで待って結果を JSON で表示
  portal.py show ID              # 今の状態を JSON で表示

環境変数: PORTAL_COOKIE（ブラウザの Cookie ヘッダの値）、PORTAL_TARGET_ID（既定 1707 = asia1）
"""
import json
import os
import re
import sys
import time
import urllib.error
import urllib.request

BASE = os.environ.get("PORTAL_BASE", "https://portal.isucon.net")
TARGET_ID = int(os.environ.get("PORTAL_TARGET_ID", "1707"))
PB = "application/vnd.google.protobuf"
STATUS = {0: "PENDING", 1: "RUNNING", 2: "ERRORED", 3: "CANCELLED", 4: "FINISHED"}

_cookies = {}


def _load_cookies():
    raw = os.environ.get("PORTAL_COOKIE", "").strip()
    if not raw:
        sys.exit("PORTAL_COOKIE is not set")
    for part in raw.split(";"):
        if "=" in part:
            k, v = part.strip().split("=", 1)
            _cookies[k] = v


def _request(method, path, body=None, headers=None):
    if not _cookies:
        _load_cookies()
    h = {
        "Cookie": "; ".join(f"{k}={v}" for k, v in _cookies.items()),
        "Accept": f"application/protobuf, {PB}, text/plain",
        "User-Agent": "isucon26-ci-bench",
    }
    h.update(headers or {})
    req = urllib.request.Request(BASE + path, data=body, method=method, headers=h)
    try:
        resp = urllib.request.urlopen(req, timeout=30)
        status, data, hdrs = resp.status, resp.read(), resp.headers
    except urllib.error.HTTPError as e:
        status, data, hdrs = e.code, e.read(), e.headers
    # Rails がセッション Cookie を更新したら追従する
    for sc in hdrs.get_all("Set-Cookie") or []:
        k, _, rest = sc.partition("=")
        _cookies[k.strip()] = rest.split(";", 1)[0]
    return status, data, hdrs


# --- 最小限の protobuf デコーダ ---

def _varint(b, i):
    r = s = 0
    while True:
        x = b[i]
        i += 1
        r |= (x & 0x7F) << s
        s += 7
        if not x & 0x80:
            return r, i


def _fields(b):
    """{field_number: [value, ...]}。length-delimited は bytes のまま返す。"""
    out, i = {}, 0
    while i < len(b):
        key, i = _varint(b, i)
        f, t = key >> 3, key & 7
        if t == 0:
            v, i = _varint(b, i)
        elif t == 2:
            n, i = _varint(b, i)
            v, i = b[i:i + n], i + n
        elif t == 1:
            v, i = b[i:i + 8], i + 8
        elif t == 5:
            v, i = b[i:i + 4], i + 4
        else:
            raise ValueError(f"wire type {t}")
        out.setdefault(f, []).append(v)
    return out


def _one(fs, n, default=None):
    return fs.get(n, [default])[0]


def _ts(b):
    if b is None:
        return None
    fs = _fields(b)
    return _one(fs, 1, 0) + _one(fs, 2, 0) / 1e9


def _str(b):
    return b.decode("utf-8", "replace") if b is not None else ""


def _job(b):
    """BenchmarkJob（ポータルの画面と突き合わせて番号を確認済み）。"""
    fs = _fields(b)
    job = {
        "id": _one(fs, 1),
        "target_id": _one(fs, 3),
        "status": STATUS.get(_one(fs, 4, 0), str(_one(fs, 4, 0))),
        "created_at": _ts(_one(fs, 5)),
        "started_at": _ts(_one(fs, 7)),
        "finished_at": _ts(_one(fs, 8)),
        "score": _one(fs, 9, 0),
    }
    res = _one(fs, 18)
    if res is not None:
        rf = _fields(res)
        bd = _fields(_one(rf, 4, b""))
        ex = _fields(_one(rf, 5, b""))
        job["result"] = {
            "finished": bool(_one(rf, 1, 0)),
            "passed": bool(_one(rf, 2, 0)),
            "score": _one(rf, 3, 0),
            "raw": _one(bd, 1, 0),
            "deduction": _one(bd, 2, 0),
            "reason": _str(_one(ex, 1)),
            "stdout": _str(_one(ex, 2)),
        }
        # stdout の末尾はベンチャーの JSON {"pass", "score", "messages"}
        m = re.search(r"\n(\{\n.*\})\s*$", job["result"]["stdout"], re.S)
        try:
            job["result"]["messages"] = json.loads(m.group(1)).get("messages", []) if m else []
        except ValueError:
            job["result"]["messages"] = []
    return job


def _check(status, data, what):
    if status != 200:
        # エラーは isuxportal.proto.Error（name=1, human_message=2）
        msg = data[:500]
        try:
            fs = _fields(data)
            msg = " / ".join(_str(v) for k in (1, 2) for v in fs.get(k, []))
        except Exception:
            pass
        sys.exit(f"{what}: HTTP {status}: {msg}")


def list_jobs(limit=0):
    st, data, _ = _request("GET", f"/api/contestant/benchmark_jobs?status=&limit={limit}")
    _check(st, data, "list")
    return [_job(b) for b in _fields(data).get(1, [])]


def get_job(job_id):
    st, data, _ = _request("GET", f"/api/contestant/benchmark_jobs/{job_id}")
    _check(st, data, "show")
    return _job(_one(_fields(data), 1))


def _csrf_token():
    st, data, _ = _request("GET", "/contestant/benchmark_jobs", headers={"Accept": "text/html"})
    if st != 200:
        sys.exit(f"csrf: HTTP {st}（PORTAL_COOKIE が切れていないか確認）")
    m = re.search(rb'<meta name="csrf-token" content="([^"]+)"', data)
    if not m:
        sys.exit("csrf: meta csrf-token が見つからない（未ログイン？）")
    return m.group(1).decode()


def enqueue():
    token = _csrf_token()
    body = bytes([0x08]) + _encode_varint(TARGET_ID)
    st, data, _ = _request("POST", "/api/contestant/benchmark_jobs", body=body, headers={
        "Content-Type": PB,
        "X-CSRF-Token": token,
        "Origin": BASE,
        "Referer": BASE + "/contestant/benchmark_jobs",
    })
    _check(st, data, "enqueue")
    return _job(_one(_fields(data), 1))


def _encode_varint(n):
    out = bytearray()
    while True:
        x = n & 0x7F
        n >>= 7
        if n:
            out.append(x | 0x80)
        else:
            out.append(x)
            return bytes(out)


def wait(job_id, timeout=900):
    deadline = time.time() + timeout
    last = None
    while time.time() < deadline:
        job = get_job(job_id)
        if job["status"] != last:
            print(f"job #{job_id}: {job['status']}", file=sys.stderr)
            last = job["status"]
        if job["status"] in ("FINISHED", "ERRORED", "CANCELLED"):
            return job
        time.sleep(5)
    sys.exit(f"job #{job_id}: timed out after {timeout}s")


def main():
    cmd = sys.argv[1] if len(sys.argv) > 1 else ""
    if cmd == "active":
        for j in list_jobs():
            if j["status"] in ("PENDING", "RUNNING"):
                print(j["id"])
    elif cmd == "latest-id":
        jobs = list_jobs()
        print(max(j["id"] for j in jobs) if jobs else 0)
    elif cmd == "list":
        n = int(sys.argv[2]) if len(sys.argv) > 2 else 10
        for j in sorted(list_jobs(), key=lambda j: -j["id"])[:n]:
            print(json.dumps(j, ensure_ascii=False))
    elif cmd == "enqueue":
        print(enqueue()["id"])
    elif cmd == "wait":
        print(json.dumps(wait(int(sys.argv[2])), ensure_ascii=False))
    elif cmd == "show":
        print(json.dumps(get_job(int(sys.argv[2])), ensure_ascii=False))
    else:
        sys.exit(__doc__)


if __name__ == "__main__":
    main()
