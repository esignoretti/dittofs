#!/usr/bin/env python3
"""Scores a run of night.sh: score.py RUN_DIR REPO.

Reads the fio results under RUN_DIR/r<repeat>/<target>/ and writes RUN_DIR/points.csv (one row per
point, target and repeat) and RUN_DIR/summary.json (CTRF: the steps are its tests, the score is in
its extra), then prints the Slack summary: the first line is the message, the rest its details.

A point's value on a target is the median of its repeats. Its ratio is DittoFS's performance over
Samba's: a throughput as it is, a time or a latency inverted, so above 1 DittoFS is faster. The
score is the geometric mean of the ratios, over every point and per area.

Flags need a noise band, which is set once from the calibration nights (CALIBRATION=yes) and kept
in band.json beside the nights: a point is flagged when its ratio falls below its 7-day median by
more than its band, and a flag or a failure seen in the 7 nights before is not reported as new.
"""
import csv
import glob
import json
import math
import os
import re
import statistics
import subprocess
import sys
import time

TARGETS = ("dittofs", "samba")
MIN_BAND = 0.05
FILE_SIZES = {"4k": 4096, "64k": 65536, "1792k": 1792 * 1024}
# The mailbox scenarios: how each one scores (see the simulator's README).
MAILBOX = {
    "logon-outlook-start": "wall",
    "mail-flow": "p99",
    "folder-resort": "wall",
    "search-reindex": "ost-read",
    "ost-full-sync": "p99",
    "logoff-compaction": "wall",
}


def load(path):
    with open(path) as f:
        return json.load(f)


def errors(doc):
    return [j["jobname"] for j in doc["jobs"] if j.get("error")]


def p99_ms(doc):
    """The 99th percentile latency of every I/O in a json+ result, reads and writes together."""
    bins = {}
    for job in doc["jobs"]:
        for side in ("read", "write"):
            lat = job[side].get("lat_ns") or job[side].get("clat_ns") or {}
            for ns, n in (lat.get("bins") or {}).items():
                bins[int(ns)] = bins.get(int(ns), 0) + n
    total, seen = sum(bins.values()), 0
    for ns in sorted(bins):
        seen += bins[ns]
        if seen >= 0.99 * total:
            return ns / 1e6
    return None


def measure(target_dir):
    """Every point of one target in one repeat: {name: (area, value, unit, higher_is_better)}, failures."""
    points, failures = {}, []
    for path in sorted(glob.glob(f"{target_dir}/matrix/*.json")):
        name = os.path.basename(path)[:-5]
        if name == "layout":
            continue
        doc = load(path)
        failures += errors(doc)
        side = "read" if "-read-" in name else "write"
        area = "sequential" if name.startswith("seq-") else "random"
        points[name] = (area, doc["jobs"][0][side]["bw_bytes"] / 2**20, "MiB/s", True)
    for path in sorted(glob.glob(f"{target_dir}/files/*.json")):
        name = os.path.basename(path)[:-5]
        doc = load(path)
        failures += errors(doc)
        size = FILE_SIZES[name.split("-")[1]]
        side = "write" if name.endswith("-create") else "read"
        points[name] = ("small files", doc["jobs"][0][side]["bw_bytes"] / size, "files/s", True)
    walls = {}
    if os.path.exists(f"{target_dir}/mailbox/wall.tsv"):
        with open(f"{target_dir}/mailbox/wall.tsv") as f:
            walls = dict(line.rstrip("\n").split("\t") for line in f if line.strip())
    for path in sorted(glob.glob(f"{target_dir}/mailbox/[0-9][0-9]-*.json")):
        m = re.match(r"\d\d-(.+)-u(\d+)-\d{8}-\d{6}\.json$", os.path.basename(path))
        if not m or m.group(1) not in MAILBOX:
            continue
        name = f"mailbox-{m.group(1)}-u{m.group(2)}"
        doc = load(path)
        failures += [f"{name} {j}" for j in errors(doc)]
        how = MAILBOX[m.group(1)]
        if how == "wall" and name in walls:
            points[name] = ("mailbox", float(walls[name]), "s", False)
        elif how == "p99" and p99_ms(doc):
            points[name] = ("mailbox", p99_ms(doc), "ms p99", False)
        elif how == "ost-read":
            bw = sum(j["read"]["bw_bytes"] for j in doc["jobs"] if j["jobname"] == "ost-read")
            points[name] = ("mailbox", bw / 2**20, "MiB/s", True)
    return points, failures


def geomean(values):
    values = [v for v in values if v and v > 0]
    return math.exp(sum(map(math.log, values)) / len(values)) if values else None


def label(name):
    m = re.match(r"(seq|rand)-(read|write)-(\w+)-t(\d+)$", name)
    if m:
        kind = "sequential" if m.group(1) == "seq" else "random"
        n = int(m.group(4))
        return f"{kind} {m.group(2)} {m.group(3).upper()} x {n} thread{'s' if n > 1 else ''}"
    m = re.match(r"files-(\w+)-(create|read)$", name)
    if m:
        size = "1.75M" if m.group(1) == "1792k" else m.group(1).upper()
        return f"{'creating' if m.group(2) == 'create' else 'reading'} {size} files"
    m = re.match(r"mailbox-(.+)-u(\d+)$", name)
    if m:
        return f"mailbox {m.group(1).split('-')[0]}, {m.group(2)} user{'s' if m.group(2) != '1' else ''}"
    return name


def pct(x):
    return f"{x * 100:.0f}%" if x is not None else "n/a"


def history(work, night):
    """The summaries of the nights before this one, oldest first."""
    out = []
    for path in sorted(glob.glob(f"{work}/20[0-9][0-9]-[0-9][0-9]-[0-9][0-9]/summary.json")):
        if os.path.basename(os.path.dirname(path)) < night:
            out.append(load(path)["results"]["extra"])
    return out


def band(work, nights):
    """The noise band per point and for the score, from the calibration nights' repeats."""
    path = f"{work}/band.json"
    if os.path.exists(path):
        return load(path)
    calibration = [n for n in nights if n.get("mode") == "calibration"]
    if not calibration:
        return None
    spreads = {}
    for n in calibration:
        for name, p in n["points"].items():
            spreads.setdefault(name, []).extend(p["repeat_ratios"])
        spreads.setdefault("score", []).extend(n["repeat_scores"])
    result = {}
    for name, ratios in spreads.items():
        mid = statistics.median(ratios)
        dev = sorted(abs(r / mid - 1) for r in ratios)
        result[name] = max(MIN_BAND, dev[int(0.9 * (len(dev) - 1))])
    with open(path, "w") as f:
        json.dump(result, f, indent=1, sort_keys=True)
    return result


def drive_wear(run):
    """Per drive: GB written during the run and the wear indicator, from nvme smart-log."""
    out = {}
    for after in glob.glob(f"{run}/nvme-after-*.json"):
        dev = after.rsplit("-", 1)[1][:-5]
        try:
            a, b = load(after), load(f"{run}/nvme-before-{dev}.json")
            # A data unit is 1,000 blocks of 512 bytes.
            written = (a["data_units_written"] - b["data_units_written"]) * 512000 / 1e9
            out[dev] = {"written_gb": round(written, 1), "percent_used": a.get("percent_used", a.get("percentage_used"))}
        except (OSError, KeyError, ValueError):
            continue
    return out


def main():
    run, repo = sys.argv[1], sys.argv[2]
    work, night = os.path.dirname(run), os.path.basename(run)
    mode = "smoke" if os.environ.get("SMOKE") == "yes" else "calibration" if os.environ.get("CALIBRATION") == "yes" else "night"

    steps = []
    with open(f"{run}/steps.tsv") as f:
        steps = list(csv.DictReader(f, delimiter="\t"))
    failures = [f"{s['step']} {s['target']} r{s['repeat']}: {s['status']}" for s in steps if s["status"] != "ok"]

    # values[name][target] = [value per repeat]; meta[name] = (area, unit, higher_is_better)
    values, meta, rows = {}, {}, []
    for rdir in sorted(glob.glob(f"{run}/r[0-9]*")):
        repeat = rdir.rsplit("/r", 1)[1]
        for target in TARGETS:
            points, failed = measure(f"{rdir}/{target}")
            failures += [f"fio {target} r{repeat}: {j} error" for j in failed]
            for name, (area, value, unit, higher) in points.items():
                values.setdefault(name, {t: {} for t in TARGETS})[target][repeat] = value
                meta[name] = (area, unit, higher)
                rows.append([night, repeat, target, name, area, f"{value:.4g}", unit, "higher" if higher else "lower"])
    with open(f"{run}/points.csv", "w", newline="") as f:
        w = csv.writer(f)
        w.writerow(["night", "repeat", "target", "point", "area", "value", "unit", "better"])
        w.writerows(rows)

    def perf(name, v):
        return v if meta[name][2] else 1 / v

    points, repeat_scores = {}, {}
    for name, by in values.items():
        d, s = by["dittofs"], by["samba"]
        if not d or not s or not all(d.values()) or not all(s.values()):
            continue
        ratio = perf(name, statistics.median(d.values())) / perf(name, statistics.median(s.values()))
        per_repeat = [perf(name, d[r]) / perf(name, s[r]) for r in sorted(d) if r in s]
        for r in d:
            if r in s:
                repeat_scores.setdefault(r, []).append(perf(name, d[r]) / perf(name, s[r]))
        points[name] = {"area": meta[name][0], "unit": meta[name][1], "better": "higher" if meta[name][2] else "lower",
                        "dittofs": list(d.values()), "samba": list(s.values()), "ratio": ratio, "repeat_ratios": per_repeat}
    score = geomean(p["ratio"] for p in points.values())
    areas = {a: geomean(p["ratio"] for p in points.values() if p["area"] == a)
             for a in ("sequential", "random", "small files", "mailbox")}

    nights = history(work, night)[-7:] if mode != "smoke" else []
    bands = band(work, nights) if mode == "night" else None
    seen_flags = {f for n in nights for f in n.get("flags", [])}
    seen_failures = {re.sub(r" r\d+:", ":", f) for n in nights for f in n.get("failures", [])}
    flags = []
    if bands and nights:
        for name, p in points.items():
            past = [n["points"][name]["ratio"] for n in nights if name in n.get("points", {})]
            if past and name in bands and p["ratio"] < statistics.median(past) * (1 - bands[name]):
                flags.append(name)
        past = [n["score"] for n in nights if n.get("score")]
        if past and score and "score" in bands and score < statistics.median(past) * (1 - bands["score"]):
            flags.append("score")
    new_flags = [f for f in flags if f not in seen_flags]
    new_failures = [f for f in failures if re.sub(r" r\d+:", ":", f) not in seen_failures]

    commit = open(f"{run}/commit").read().strip() if os.path.exists(f"{run}/commit") else ""
    since = next((n.get("commit") for n in reversed(nights) if n.get("commit")), "")
    commits = ""
    if commit and since and since != commit:
        count = subprocess.run(["git", "-C", repo, "rev-list", "--count", f"{since}..{commit}"],
                               capture_output=True, text=True).stdout.strip()
        commits = f" ({count or '?'} commits since `{since[:9]}`)"

    extra = {"night": night, "mode": mode, "commit": commit, "score": score, "areas": areas, "points": points,
             "repeat_scores": [geomean(v) for _, v in sorted(repeat_scores.items())],
             "flags": flags, "failures": failures, "drives": drive_wear(run)}
    tests = [{"name": f"{s['step']} {s['target']} r{s['repeat']}".replace(" - r-", ""), "suite": ["ditto-nightly"],
              "status": {"ok": "passed", "skipped": "skipped"}.get(s["status"], "failed"), "rawStatus": s["status"],
              "duration": int(float(s["seconds"]) * 1000)} for s in steps]
    started = int(open(f"{run}/started").read()) * 1000 if os.path.exists(f"{run}/started") else 0
    summary = {"tests": len(tests), "passed": sum(t["status"] == "passed" for t in tests),
               "failed": sum(t["status"] == "failed" for t in tests), "skipped": sum(t["status"] == "skipped" for t in tests),
               "pending": 0, "other": 0, "start": started, "stop": int(time.time() * 1000)}
    with open(f"{run}/summary.json", "w") as f:
        json.dump({"reportFormat": "CTRF", "specVersion": "0.0.0", "generatedBy": "bench/ditto/score.py",
                   "results": {"tool": {"name": "ditto-nightly"}, "summary": summary, "tests": tests,
                               "environment": {"commit": commit, "branchName": "develop", "osPlatform": "linux"},
                               "extra": extra}}, f, indent=1)

    # The Slack summary.
    if score is None:
        title = f":x: Nightly performance: nothing to score ({mode})"
    else:
        past = [n["score"] for n in nights if n.get("score")]
        trend = f"; was {pct(past[-1])} last night, 7-day median {pct(statistics.median(past))}" if past else ""
        mark = ":warning: " if new_flags or new_failures else ""
        title = f"{mark}DittoFS at {pct(score)} of Samba (geometric mean of {len(points)} points{trend})"
    lines = []
    if points:
        worst = min(points, key=lambda n: points[n]["ratio"])
        line = f"Worst: {label(worst)} at {pct(points[worst]['ratio'])}."
        logons = sorted((n for n in points if n.startswith("mailbox-logon")), key=lambda n: int(n.rsplit("-u", 1)[1]))
        if logons:
            n = logons[-1]
            line += f" Mailbox, {n.rsplit('-u', 1)[1]} users: logon {1 / points[n]['ratio']:.1f}x Samba's time."
        lines.append(line)
        lines.append(" · ".join(f"{a} {pct(v)}" for a, v in areas.items() if v is not None).capitalize())
    if mode == "night" and not bands:
        lines.append("No noise band yet, so no flags: it comes from the calibration nights.")
    elif flags:
        old = len(flags) - len(new_flags)
        lines.append("Below their 7-day median: " + (", ".join(label(f) for f in new_flags) or "nothing new")
                     + (f" ({old} already flagged this week)" if old else ""))
    if failures:
        old = len(failures) - len(new_failures)
        lines.append("Failed: " + ("; ".join(new_failures) or "nothing new") + (f" ({old} failing this week already)" if old else ""))
    wear = ", ".join(f"{d} {w['written_gb']} GB written, {w['percent_used']}% used" for d, w in extra["drives"].items())
    host = os.environ.get("SCENARIO_HOST") or os.uname().nodename.split(".")[0]
    lines.append(" · ".join(x for x in (f"develop `{commit[:9]}`{commits}" if commit else "", host, wear, f"`{run}/`") if x))
    print("\n".join([title] + lines))


if __name__ == "__main__":
    main()
