#!/usr/bin/env python3
"""Download and normalize T1 data for the Node ecosystem (standard library only).

Sources:
    - SWE-bench/SWE-bench_Multilingual (datasets-server rows API), keeping JS/TS repos.
    - ByteDance-Seed/Multi-SWE-bench js/ and ts/ directories (JSONL downloaded directly, read line by line, raw files not stored).
Output: meta.json (HF revision) and normalized JSONL {instance_id, repo, base_commit, patch, test_patch, FAIL_TO_PASS, source}.

  python fetch_datasets.py OUTDIR
"""
import json, sys, os, urllib.request, urllib.parse

ML_JS = {"preactjs/preact", "axios/axios", "babel/babel", "facebook/docusaurus", "vuejs/core",
         "mrdoob/three.js", "immutable-js/immutable-js"}
HF = "https://huggingface.co/api/datasets/ByteDance-Seed/Multi-SWE-bench/tree/main/"
RAW = "https://huggingface.co/datasets/ByteDance-Seed/Multi-SWE-bench/resolve/main/"


def get(url):
    return urllib.request.urlopen(urllib.request.Request(url, headers={"User-Agent": "bgraph-eval"}), timeout=120)


def multilingual(out):
    n = 0
    for off in (0, 100, 200):
        u = ("https://datasets-server.huggingface.co/rows?dataset=SWE-bench/SWE-bench_Multilingual"
             f"&config=default&split=test&offset={off}&length=100")
        for r in json.load(get(u))["rows"]:
            r = r["row"]
            if r["repo"] not in ML_JS:
                continue
            out.append({"instance_id": r["instance_id"], "repo": r["repo"], "base_commit": r["base_commit"],
                        "patch": r["patch"], "test_patch": r["test_patch"], "FAIL_TO_PASS": r["FAIL_TO_PASS"],
                        "source": "SWE-bench_Multilingual"})
            n += 1
    print("multilingual", n)


def multiswe(out):
    for lang in ("js", "ts"):
        files = json.load(get(HF + lang))
        for f in files:
            p = f["path"]
            if not p.endswith(".jsonl"):
                continue
            n = 0
            with get(RAW + urllib.parse.quote(p)) as resp:
                for line in resp:
                    line = line.strip()
                    if not line:
                        continue
                    r = json.loads(line)
                    f2p = r.get("f2p_tests") or {}
                    out.append({"instance_id": r.get("instance_id") or f'{r["org"]}__{r["repo"]}-{r["number"]}',
                                "repo": f'{r["org"]}/{r["repo"]}', "base_commit": r["base"]["sha"],
                                "patch": r.get("fix_patch", ""), "test_patch": r.get("test_patch", ""),
                                "FAIL_TO_PASS": list(f2p.keys()) if isinstance(f2p, dict) else f2p,
                                "source": "Multi-SWE-bench"})
                    n += 1
            print(p, n, flush=True)


def meta(d):
    m = {}
    for name in ("SWE-bench/SWE-bench_Multilingual", "ByteDance-Seed/Multi-SWE-bench"):
        m[name] = json.load(get("https://huggingface.co/api/datasets/" + name))["sha"]
    with open(os.path.join(d, "meta.json"), "w", encoding="utf-8") as fh:
        json.dump(m, fh, indent=1)


if __name__ == "__main__":
    d = sys.argv[1]
    os.makedirs(d, exist_ok=True)
    meta(d)
    rows = []
    multilingual(rows)
    multiswe(rows)
    with open(os.path.join(d, "tasks.jsonl"), "w", encoding="utf-8", newline="\n") as fh:
        for r in rows:
            fh.write(json.dumps(r, ensure_ascii=False) + "\n")
    print("total", len(rows))
