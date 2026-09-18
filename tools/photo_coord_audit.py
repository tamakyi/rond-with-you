#!/usr/bin/env python3
"""照片导入的坐标体检与批量换算。

为什么需要它：导入页把「用户选的坐标系」交给 /api/regeo（coord 字段），服务端只对 wgs84
做换算，坐标再 toFixed(6) 回填表单落库。选错一次，显示坐标就一字不差等于 EXIF 原值，在
GCJ-02 底图上偏 350~680m。库里没有 created_at，也没记录导入时的坐标系选择，所以判据只有
原图 EXIF：

    库内坐标 == round(EXIF, 6)        -> 没转换，需要修
    库内坐标 == gcj(round(EXIF, 6))   -> 已转换，正确
    （实测 24/24 命中，0 反例）

没有原图时退化为「sp > rond 基线上限 = 站点新建」，只能靠 --include-unverified 才动。

用法：
    python tools/photo_coord_audit.py --site https://life.shiroko.one --cookie "uid|tok" \
        --photos D:/photos/libo --photos D:/photos/foshan --out fix.csv
    # 确认清单后加 --apply
"""
import argparse
import csv
import html as htmllib
import io
import json
import math
import os
import re
import socket
import sys
import time
import urllib.error
import urllib.parse
import urllib.request

# 生产机只监听 IPv4：默认解析出 IPv6 会先撞在超时上，而 urllib 不会自动回退
_gai = socket.getaddrinfo


def _gai_prefer_v4(host, port, family=0, type=0, proto=0, flags=0):
    infos = _gai(host, port, family, type, proto, flags)
    v4 = [i for i in infos if i[0] == socket.AF_INET]
    return v4 or infos


socket.getaddrinfo = _gai_prefer_v4

UA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) rond-coord-audit"
FORM_RE = re.compile(r'<form method="post" action="/admin/edit/place/update".*?</form>', re.S)
ORIG_RE = re.compile(r'导入时的坐标：<span class="mono">([-0-9.]+),\s*([-0-9.]+)</span>')


class _NoRedirect(urllib.request.HTTPRedirectHandler):
    # 保存成功是 303 + Location 里带 ok=/err=，跟过去就看不到结论了；
    # 会话失效也是 303 跳登录页，跟着走会静默拿到登录页 HTML 当成空数据
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        return None


_opener = urllib.request.build_opener(_NoRedirect)


def http(url, cookie="", data=None, timeout=60, tries=3):
    # 走 Cloudflare 时长页面偶发读超时，重试比直接失败划算；写库的重试由调用方控
    last = None
    for i in range(tries):
        req = urllib.request.Request(url, data=data)
        req.add_header("User-Agent", UA)
        if cookie:
            req.add_header("Cookie", "rond_session=" + cookie)
        if data is not None:
            req.add_header("Content-Type", "application/x-www-form-urlencoded")
        try:
            with _opener.open(req, timeout=timeout) as r:
                return r.status, dict(r.headers), r.read()
        except urllib.error.HTTPError as e:
            return e.code, dict(e.headers), e.read()
        except Exception as e:            # 超时 / 连接被掐
            last = e
            if i + 1 < tries:
                time.sleep(1.5 * (i + 1))
    raise RuntimeError("请求失败 %s：%s" % (url, last))


def field(block, name):
    m = re.search(r'name="%s"[^>]*?value="([^"]*)"' % re.escape(name), block)
    return htmllib.unescape(m.group(1)) if m else ""


def parse_places(page_html):
    out = []
    for block in FORM_RE.findall(page_html):
        src = field(block, "src_pk")
        if not src.isdigit():
            continue
        cat = ""
        sel = re.search(r'<select name="category".*?</select>', block, re.S)
        if sel:
            cur = re.search(r'<option value="([^"]*)" selected>', sel.group(0))
            cat = htmllib.unescape(cur.group(1)) if cur else ""
        p = {
            "src_pk": int(src),
            "name": field(block, "name"),
            "category": cat,
            "lat": field(block, "lat"),
            "lon": field(block, "lon"),
            "province": field(block, "province"),
            "city": field(block, "city"),
            "district": field(block, "district"),
        }
        mo = ORIG_RE.search(block)
        p["orig"] = (mo.group(1) + "," + mo.group(2)) if mo else ""
        out.append(p)
    return out


def fetch_places(site, cookie, verbose=True):
    """翻页读完整个地点列表（含被区域黑名单/迷雾遮罩挡住、公开接口看不到的点）。"""
    places, seen, page = [], set(), 1
    while page <= 200:
        status, _, body = http("%s/admin/edit?page=%d" % (site, page), cookie)
        if status != 200:
            raise SystemExit("读 /admin/edit?page=%d 失败：HTTP %d（会话过期？）" % (page, status))
        chunk = parse_places(body.decode("utf-8", "replace"))
        chunk = [p for p in chunk if p["src_pk"] not in seen]
        if not chunk:
            break
        for p in chunk:
            seen.add(p["src_pk"])
        places.extend(chunk)
        if verbose:
            sys.stderr.write("\r读取地点列表… 第 %d 页，累计 %d 个" % (page, len(places)))
            sys.stderr.flush()
        page += 1
    if verbose:
        sys.stderr.write("\n")
    return places


def wgs2gcj(lat, lon):
    a, ee = 6378245.0, 0.00669342162296594323

    def tl(x, y):
        r = -100.0 + 2.0 * x + 3.0 * y + 0.2 * y * y + 0.1 * x * y + 0.2 * math.sqrt(abs(x))
        r += (20.0 * math.sin(6 * x * math.pi) + 20.0 * math.sin(2 * x * math.pi)) * 2 / 3
        r += (20.0 * math.sin(y * math.pi) + 40.0 * math.sin(y / 3 * math.pi)) * 2 / 3
        r += (160.0 * math.sin(y / 12 * math.pi) + 320 * math.sin(y * math.pi / 30)) * 2 / 3
        return r

    def tg(x, y):
        r = 300.0 + x + 2.0 * y + 0.1 * x * x + 0.1 * x * y + 0.1 * math.sqrt(abs(x))
        r += (20.0 * math.sin(6 * x * math.pi) + 20.0 * math.sin(2 * x * math.pi)) * 2 / 3
        r += (20.0 * math.sin(x * math.pi) + 40.0 * math.sin(x / 3 * math.pi)) * 2 / 3
        r += (150.0 * math.sin(x / 12 * math.pi) + 300.0 * math.sin(x / 30 * math.pi)) * 2 / 3
        return r

    dl, dn = tl(lon - 105.0, lat - 35.0), tg(lon - 105.0, lat - 35.0)
    rl = lat / 180 * math.pi
    m = math.sin(rl)
    m = 1 - ee * m * m
    sm = math.sqrt(m)
    return (round(lat + (dl * 180) / ((a * (1 - ee)) / (m * sm) * math.pi), 6),
            round(lon + (dn * 180) / (a / sm * math.cos(rl) * math.pi), 6))


def haversine(a, b, c, d):
    R = 6371.0088
    p1, p2 = math.radians(a), math.radians(c)
    dp, dl = math.radians(c - a), math.radians(d - b)
    x = math.sin(dp / 2) ** 2 + math.cos(p1) * math.cos(p2) * math.sin(dl / 2) ** 2
    return 2 * R * math.asin(math.sqrt(x)) * 1000


def read_exif(dirs):
    """读出照片目录里所有带 GPS 的坐标（四舍五入到 6 位，与导入页 toFixed(6) 对齐）。"""
    try:
        from PIL import Image
    except ImportError:
        raise SystemExit("需要 Pillow：pip install pillow")
    try:
        import pillow_heif
        pillow_heif.register_heif_opener()
    except ImportError:
        pass
    pts, skipped = [], 0
    for root_dir in dirs:
        for root, _, files in os.walk(root_dir):
            for fn in sorted(files):
                if not fn.lower().endswith((".jpg", ".jpeg", ".heic", ".heif", ".png")):
                    continue
                path = os.path.join(root, fn)
                try:
                    ex = Image.open(path).getexif()
                    g = ex.get_ifd(34853)
                    la, lo = g.get(2), g.get(4)
                    if not la or not lo:
                        skipped += 1
                        continue
                    lat = float(la[0]) + float(la[1]) / 60 + float(la[2]) / 3600
                    lon = float(lo[0]) + float(lo[1]) / 60 + float(lo[2]) / 3600
                    if str(g.get(1)).upper().startswith("S"):
                        lat = -lat
                    if str(g.get(3)).upper().startswith("W"):
                        lon = -lon
                    pts.append({"file": fn, "lat": round(lat, 6), "lon": round(lon, 6),
                                "taken": (ex.get(36867) or ex.get(306) or "")})
                except Exception:
                    skipped += 1
    return pts, skipped


def classify(place, photos):
    try:
        lat, lon = round(float(place["lat"]), 6), round(float(place["lon"]), 6)
    except ValueError:
        return "invalid", None
    for ph in photos:
        if ph["lat"] == lat and ph["lon"] == lon:
            return "raw", ph
    for ph in photos:
        if wgs2gcj(ph["lat"], ph["lon"]) == (lat, lon):
            return "gcj", ph
    return "unknown", None


def api_coords(site, cookie, lat, lon):
    q = urllib.parse.urlencode({"mode": "wgs2gcj", "lat": lat, "lon": lon})
    status, _, body = http("%s/api/coords?%s" % (site, q), cookie)
    if status != 200:
        raise RuntimeError("换算接口 HTTP %d" % status)
    d = json.loads(body.decode("utf-8"))
    if d.get("error"):
        raise RuntimeError(d["error"])
    return d


def save_place(site, cookie, p, lat, lon):
    form = urllib.parse.urlencode({
        "src_pk": p["src_pk"], "name": p["name"], "category": p["category"],
        "lat": "%.6f" % lat, "lon": "%.6f" % lon,
        "province": p["province"], "city": p["city"], "district": p["district"],
    }).encode("utf-8")
    status, headers, _ = http("%s/admin/edit/place/update" % site, cookie, data=form, timeout=60)
    loc = headers.get("Location", "")
    if status not in (302, 303):
        return False, "HTTP %d" % status
    if "err=" in loc:
        return False, urllib.parse.unquote(loc.split("err=")[-1])
    return True, urllib.parse.unquote(loc.split("ok=")[-1]) if "ok=" in loc else "ok"


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--site", required=True, help="站点根地址，如 https://life.shiroko.one")
    ap.add_argument("--cookie", default=os.environ.get("ROND_COOKIE", ""),
                    help="rond_session 的值（也可用环境变量 ROND_COOKIE）")
    ap.add_argument("--photos", action="append", default=[], help="原图目录，可重复")
    ap.add_argument("--baseline-max-sp", type=int, default=323,
                    help="rond 基线里 ZLOCATION 的 Z_PK 上限，大于它的都是站点新建")
    ap.add_argument("--out", default="photo-coord-fix.csv")
    ap.add_argument("--apply", action="store_true", help="真的写库（不加则只出清单）")
    ap.add_argument("--include-unverified", action="store_true",
                    help="连没有原图可核对的点一起换算（风险自负）")
    ap.add_argument("--only", default="", help="只处理这些 src_pk，逗号分隔")
    ap.add_argument("--sleep", type=float, default=0.15, help="每次写库之间的间隔秒数")
    args = ap.parse_args()

    site = args.site.rstrip("/")
    if not args.cookie:
        raise SystemExit("缺少 --cookie（浏览器登录后复制 rond_session 的值）")

    places = fetch_places(site, args.cookie)
    created = [p for p in places if p["src_pk"] > args.baseline_max_sp]
    print("地点总数 %d，其中站点新建（sp > %d）%d 个"
          % (len(places), args.baseline_max_sp, len(created)))

    photos = []
    if args.photos:
        photos, skipped = read_exif(args.photos)
        print("原图：读到 %d 张带 GPS 的照片（%d 张无 GPS/读不了）" % (len(photos), skipped))

    rows = []
    for p in created:
        verdict, ph = classify(p, photos)
        new = wgs2gcj(round(float(p["lat"]), 6), round(float(p["lon"]), 6))
        moved = haversine(round(float(p["lat"]), 6), round(float(p["lon"]), 6), new[0], new[1])
        rows.append({**p, "verdict": verdict, "photo": ph["file"] if ph else "",
                     "new_lat": new[0], "new_lon": new[1], "moved": moved})

    todo = [r for r in rows if r["verdict"] == "raw"]
    if args.include_unverified:
        todo += [r for r in rows if r["verdict"] == "unknown"]
    if args.only:
        want = {int(x) for x in args.only.replace(" ", "").split(",") if x}
        todo = [r for r in todo if r["src_pk"] in want]

    n_raw = sum(1 for r in rows if r["verdict"] == "raw")
    n_gcj = sum(1 for r in rows if r["verdict"] == "gcj")
    n_unk = sum(1 for r in rows if r["verdict"] == "unknown")
    print("判定：没转换 %d ｜ 已正确 %d ｜ 无原图可核 %d" % (n_raw, n_gcj, n_unk))

    with io.open(args.out, "w", encoding="utf-8-sig", newline="") as f:
        w = csv.writer(f)
        w.writerow(["src_pk", "名称", "城市", "判定", "原图", "现在纬度", "现在经度",
                    "换算后纬度", "换算后经度", "移动(米)"])
        for r in sorted(rows, key=lambda x: x["src_pk"]):
            w.writerow([r["src_pk"], r["name"], r["city"],
                        {"raw": "没转换", "gcj": "已正确", "unknown": "无原图", "invalid": "坐标无效"}[r["verdict"]],
                        r["photo"], r["lat"], r["lon"],
                        "%.6f" % r["new_lat"], "%.6f" % r["new_lon"], "%.0f" % r["moved"]])
    print("清单已写：%s" % args.out)
    for r in todo[:10]:
        print("  sp=%-5s %-20s %s,%s → %.6f,%.6f  (%.0fm)"
              % (r["src_pk"], r["name"][:18], r["lat"], r["lon"], r["new_lat"], r["new_lon"], r["moved"]))
    if len(todo) > 10:
        print("  …共 %d 条" % len(todo))

    if not args.apply:
        print("\n[dry-run] 加 --apply 才会写库。")
        return
    if not todo:
        print("\n没有需要处理的点。")
        return

    applied, failed = [], []
    for i, r in enumerate(todo, 1):
        try:
            d = api_coords(site, args.cookie, r["lat"], r["lon"])
            ok, msg = save_place(site, args.cookie, r, d["lat"], d["lon"])
        except Exception as e:
            ok, msg, d = False, str(e), None
        if ok:
            applied.append({**r, "applied_lat": "%.6f" % d["lat"], "applied_lon": "%.6f" % d["lon"]})
            sys.stderr.write("\r已改 %d/%d  sp=%s %s        " % (i, len(todo), r["src_pk"], msg[:40]))
        else:
            failed.append({**r, "err": msg})
            sys.stderr.write("\r失败 %d/%d  sp=%s %s        " % (i, len(todo), r["src_pk"], msg[:40]))
        sys.stderr.flush()
        time.sleep(args.sleep)
    print()

    log = args.out + ".applied.csv"
    with io.open(log, "w", encoding="utf-8-sig", newline="") as f:
        w = csv.writer(f)
        w.writerow(["src_pk", "名称", "原纬度", "原经度", "新纬度", "新经度", "回退用经度", "回退用纬度"])
        for r in applied:
            w.writerow([r["src_pk"], r["name"], r["lat"], r["lon"],
                        r["applied_lat"], r["applied_lon"], r["lat"], r["lon"]])
    print("成功 %d 条，失败 %d 条；回退清单：%s" % (len(applied), len(failed), log))
    for r in failed:
        print("  失败 sp=%s %s：%s" % (r["src_pk"], r["name"][:16], r["err"]))


if __name__ == "__main__":
    main()
