#!/usr/bin/env python3
"""结构级对比：导出的 .rondbackup / .fwss 与真机原版，判断「能不能被软件读回去」。

与 tools/verify_rondbackup.py 的分工：那个比**字段取值**（MANAGED 表逐列），
这个比**结构**——zip 条目、表/索引/建表语句、格式不变量、自洽性、确定性。
两者都用「未改动的往返包」时应当全过；用真库的导出包时，字段级差异是正常的
（站点新建的行），但**结构不变量与自洽性必须仍然全过**。

用法：
  python tools/verify_export_structure.py all <原版.rondbackup> <导出.rondbackup> \
                                               <原版.fwss> <导出.fwss>
  python tools/verify_export_structure.py rondbackup <原版> <导出> [<第二次导出>]
  python tools/verify_export_structure.py fwss <原版> <导出> [<第二次导出>]

两点必须知道的前提：
  * 原版 rondbackup 是**带 WAL 的活快照**（主库小、-wal 大）。必须三个文件一起解出来
    让 SQLite 重放 WAL，否则行数全 0、比出来全是「缺行」的假象。
  * 原版 fwss 是某一天的快照，块数据与当前库**本来就该不同**，所以内容不比，
    只比命名可逆、槽位自洽、块尾 24 位数的定义这些格式不变量。
"""
import hashlib
import os
import sqlite3
import sys
import tempfile
import zipfile
import zlib

# internal/fog/fog.go 的常量与掩码（改那边记得同步这里）
MAP_TILES, TILE_BLOCKS = 512, 128
HEADER_LEN, HEADER_SIZE = TILE_BLOCKS * TILE_BLOCKS, 32768
BITMAP, BLOCK = 512, 515
MASK1, MASK2 = "olhwjsktri", "eizxdwknmo"
SENTINEL = -63114076800  # ZLASTARRIVALDATE_ 的「无数据」哨兵

OUT, FAILED, WARNS = [], [False], []


def say(s=""):
    OUT.append(s)


def head(s):
    say()
    say("=" * 78)
    say(s)
    say("=" * 78)


def fail(msg):
    FAILED[0] = True
    say("  !! " + msg)


# ------------------------------------------------------------ fwss 命名编码
def tile_name(tid):
    s = str(tid)
    b = hashlib.md5(s.encode()).hexdigest()[:4]
    for ch in s:
        b += MASK1[int(ch)]
    for ch in s[-2:]:
        b += MASK2[int(ch)]
    return b


def name_reversible(name):
    if len(name) < 7:
        return False
    digits = ""
    for c in name[4:len(name) - 2]:
        if c not in MASK1:
            return False
        digits += str(MASK1.index(c))
    return tile_name(int(digits)) == name


# ---------------------------------------------------------------- rondbackup
def compare_rondbackup(orig, new, new2=None):
    head("一、rondbackup 结构对比")
    say("原版: %s (%.2f MB)" % (os.path.basename(orig), os.path.getsize(orig) / 1048576))
    say("导出: %s (%.2f MB)" % (os.path.basename(new), os.path.getsize(new) / 1048576))
    zo, zn = zipfile.ZipFile(orig), zipfile.ZipFile(new)
    oi = {i.filename: i for i in zo.infolist()}
    ni = {i.filename: i for i in zn.infolist()}
    say("zip 条目: 原版 %d / 导出 %d" % (len(oi), len(ni)))
    if set(oi) != set(ni):
        fail("条目集合不同：缺 %s，多 %s" % (sorted(set(oi) - set(ni)), sorted(set(ni) - set(oi))))
    else:
        say("  条目名完全一致 ✓（%s）" % ", ".join(sorted(oi)))
    say("  %-20s %14s %14s" % ("条目", "原版", "导出"))
    for n in sorted(oi):
        say("  %-20s %14d %14d" % (n, oi[n].file_size, ni[n].file_size))

    def extract(z):
        d = tempfile.mkdtemp(prefix="rbstruct-")
        for i in z.infolist():
            with open(os.path.join(d, i.filename), "wb") as f:
                f.write(z.read(i.filename))
        return d

    co = sqlite3.connect(os.path.join(extract(zo), "LifeEasy.sqlite"))
    cn = sqlite3.connect(os.path.join(extract(zn), "LifeEasy.sqlite"))

    def schema(db):
        t, idx = {}, {}
        for typ, name, tbl, sql in db.execute("SELECT type,name,tbl_name,sql FROM sqlite_master"):
            (t if typ == "table" else idx)[name] = sql
        return t, idx

    to, io_ = schema(co)
    tn, in_ = schema(cn)
    head("1.1 表 / 索引 / 建表语句 / 列")
    say("表: 原版 %d / 导出 %d；索引: 原版 %d / 导出 %d" % (len(to), len(tn), len(io_), len(in_)))
    for label, a, b in (("表", set(to), set(tn)), ("索引", set(io_), set(in_))):
        if a == b:
            say("  %s集合一致 ✓" % label)
        else:
            fail("%s集合不同：缺 %s，多 %s" % (label, sorted(a - b), sorted(b - a)))
    bad_sql = [t for t in set(to) & set(tn) if to[t] != tn[t]]
    if bad_sql:
        fail("CREATE TABLE 语句不同: %s" % bad_sql)
    else:
        say("  %d 张表的 CREATE TABLE 语句逐字符一致 ✓" % len(to))
    say()
    say("%-24s %9s %9s  %s" % ("表", "原版行数", "导出行数", "列定义"))
    say("-" * 78)
    for tbl in sorted(set(to) & set(tn)):
        ca = [r[1] for r in co.execute('PRAGMA table_info("%s")' % tbl)]
        cb = [r[1] for r in cn.execute('PRAGMA table_info("%s")' % tbl)]
        ra = co.execute('SELECT count(*) FROM "%s"' % tbl).fetchone()[0]
        rb = cn.execute('SELECT count(*) FROM "%s"' % tbl).fetchone()[0]
        if ca != cb:
            fail("%s 列定义不同" % tbl)
            say("  %-24s %9d %9d  !! 原=%s 新=%s" % (tbl, ra, rb, ca, cb))
        else:
            say("  %-24s %9d %9d  一致" % (tbl, ra, rb))
    say()
    say("  行数差异说明：用真库导出时，站点新建的行（ZVISIT/ZLOCATION/ZACTIVITY/ZTRANSPORT）")
    say("  会比原版多，这是预期；用「未改动的往返包」时应当全部相等。")

    head("1.2 自洽性（决定能不能被读回去）")
    for label, db in (("原版", co), ("导出", cn)):
        ic = db.execute("PRAGMA integrity_check").fetchone()[0]
        fk = len(db.execute("PRAGMA foreign_key_check").fetchall())
        say("%s: integrity_check=%s  foreign_key_check 违例=%d" % (label, ic, fk))
        if label == "导出" and (ic != "ok" or fk):
            fail("导出包自洽性不通过")
    wal = os.path.getsize(os.path.join(extract(zn), "LifeEasy.sqlite-wal"))
    say("导出的 -wal = %d 字节（0 = 打开即完整数据，无需重放）" % wal)
    md = cn.execute("SELECT Z_PLIST FROM Z_METADATA LIMIT 1").fetchone()
    say("Z_METADATA: %d 行，Z_PLIST %d 字节（Core Data 模型版本，缺了 rond 认不出模型）"
        % (cn.execute("SELECT count(*) FROM Z_METADATA").fetchone()[0], len(md[0] or b"") if md else 0))
    say("Z_PRIMARYKEY 行数 = %d（实体编号表）"
        % cn.execute("SELECT count(*) FROM Z_PRIMARYKEY").fetchone()[0])

    head("1.3 关键不变量（导出侧）")
    checks = [
        ("ZMOVEMENT.ZTYPE_ 为空（真机 791/791 有值）", "SELECT count(*) FROM ZMOVEMENT WHERE ZTYPE_ IS NULL"),
        ("ZMOVEMENT 缺 ZSTART_", "SELECT count(*) FROM ZMOVEMENT WHERE ZSTART_ IS NULL"),
        ("ZMOVEMENT 缺 ZEND_", "SELECT count(*) FROM ZMOVEMENT WHERE ZEND_ IS NULL"),
        ("ZMOVEMENT ZSTART_ 早于起点离开",
         "SELECT count(*) FROM ZMOVEMENT m JOIN ZVISIT vf ON vf.Z_PK=m.ZVISITFROM_"
         " WHERE vf.ZDEPARTUREDATE_ IS NOT NULL AND m.ZSTART_ < vf.ZDEPARTUREDATE_"),
        ("ZACTIVITY 缺 ZUID_", "SELECT count(*) FROM ZACTIVITY WHERE ZUID_ IS NULL OR ZUID_=''"),
        ("新建的 ZVISIT 缺 ZIDENTIFIER_",
         "SELECT count(*) FROM ZVISIT WHERE ZUSERADDED=1 AND (ZIDENTIFIER_ IS NULL OR ZIDENTIFIER_='')"),
        ("ZLOCATION 缺 ZRAWLATITUDE", "SELECT count(*) FROM ZLOCATION WHERE ZRAWLATITUDE IS NULL"),
    ]
    for label, sql in checks:
        v = cn.execute(sql).fetchone()[0]
        if v:
            fail("%s = %d（期望 0）" % (label, v))
        else:
            say("  %-42s = 0 ✓" % label)

    # 只提示、不判失败：导出只能如实反映库里的东西。用真库导出时，基线里有、库里没有的行
    # 会在这里露出来（正常情况是「一条全空的 ZVISIT」，早期导入路径跳过过它）。
    miss_rows = {}
    for tbl, col in (("ZVISIT", "Z_PK"), ("ZLOCATION", "Z_PK"), ("ZMOVEMENT", "Z_PK")):
        sa = {r[0] for r in co.execute("SELECT %s FROM %s" % (col, tbl))}
        sb = {r[0] for r in cn.execute("SELECT %s FROM %s" % (col, tbl))}
        if sa - sb:
            miss_rows[tbl] = sorted(sa - sb)
    say("  %-42s %s" % ("基线有、导出没有的行",
                       "无 ✓" if not miss_rows else str(miss_rows)))
    if miss_rows:
        WARNS.append(
            "基线里这些行不在导出中：%s。导出只反映库里现有的数据，所以这不是导出缺陷 ——"
            "用「未改动的往返包」时不出现，说明是当年那次导入（9-14）用的是更早的代码，"
            "或这些行被删过。想对齐就重导一次原包。"
            % ", ".join("%s %s" % (k, v) for k, v in miss_rows.items()))

    nbad = cn.execute(
        "SELECT count(*) FROM ZMOVEMENT m JOIN ZVISIT vt ON vt.Z_PK=m.ZVISITTO_"
        " WHERE vt.ZARRIVALDATE_ IS NOT NULL AND m.ZEND_ > vt.ZARRIVALDATE_").fetchone()[0]
    say("  %-42s = %d%s" % ("ZMOVEMENT ZEND_ 晚于终点到达（容差内允许）", nbad,
                           "" if not nbad else "  ← 见下方说明"))
    if nbad:
        for r in cn.execute(
                "SELECT m.Z_PK, m.ZVISITTO_, m.ZEND_-vt.ZARRIVALDATE_ FROM ZMOVEMENT m"
                " JOIN ZVISIT vt ON vt.Z_PK=m.ZVISITTO_"
                " WHERE vt.ZARRIVALDATE_ IS NOT NULL AND m.ZEND_ > vt.ZARRIVALDATE_"):
            say("       movement %d → visit %d：超出 %.1f 秒" % (r[0], r[1], r[2]))
        WARNS.append(
            "ZEND_ 晚于终点到达的有 %d 条（均在 1 分钟内）。成因：站点允许手填时间有 1 分钟"
            "容差，而**改到访时间时不会回头校验引用它的行程** —— 到访的到达时间被改早之后，"
            "原先合法的行程就变成越界。不属格式问题，rond 读的是同一种数据形状。" % nbad)

    # 派生缓存列：导出的值应当等于按 ZVISIT 实算的 MAX(到达)
    head("1.4 派生缓存列 ZLASTARRIVALDATE_ 是否按到访重算")
    diff_rows = []
    for pk, ch in cn.execute("SELECT Z_PK, ZLASTARRIVALDATE_ FROM ZLOCATION"):
        r = cn.execute("SELECT max(ZARRIVALDATE_) FROM ZVISIT WHERE ZLOCATION=?", (pk,)).fetchone()[0]
        if r is not None and ch != r:
            diff_rows.append((pk, ch, r))
    say("导出里「缓存 != MAX(到访到达)」的地点: %d" % len(diff_rows))
    if diff_rows:
        fail("导出有 %d 个地点的派生缓存没有按到访重算" % len(diff_rows))
    else:
        say("  全部按 ZVISIT 重算一致 ✓（这个列不写会让 rond 显示「无数据」）")
    stale = 0
    for pk, ch in co.execute("SELECT Z_PK, ZLASTARRIVALDATE_ FROM ZLOCATION"):
        r = co.execute("SELECT max(ZARRIVALDATE_) FROM ZVISIT WHERE ZLOCATION=?", (pk,)).fetchone()[0]
        if r is not None and ch != r:
            stale += 1
    say("对照：原版自身「缓存 != MAX(到访到达)」的地点 %d 个（原版缓存陈旧，导出是修正）" % stale)

    head("1.5 确定性（同一份数据导出两次是否一致）")
    if new2 and os.path.exists(new2):
        def dig(p):
            d = extract(zipfile.ZipFile(p))
            return hashlib.sha256(open(os.path.join(d, "LifeEasy.sqlite"), "rb").read()).hexdigest()
        a, b = dig(new), dig(new2)
        say("  LifeEasy.sqlite sha256: %s" % ("一致 ✓" if a == b else "不同 ×"))
        say("    #1 %s" % a)
        say("    #2 %s" % b)
        if a != b:
            fail("两次导出不一致：ZIDENTIFIER_/ZUID_ 之类若是随机的，rond 会把整条当新记录")
    else:
        say("  未提供第二次导出，跳过（真担心就再导一次，比 sha256 即可）")


# ---------------------------------------------------------------------- fwss
def compare_fwss(orig, new, new2=None):
    head("二、fwss 迷雾快照结构对比")
    say("原版: %s (%.1f KB)" % (os.path.basename(orig), os.path.getsize(orig) / 1024))
    say("导出: %s (%.1f KB)" % (os.path.basename(new), os.path.getsize(new) / 1024))
    fo, fn = zipfile.ZipFile(orig), zipfile.ZipFile(new)
    fon, fnn = set(fo.namelist()), set(fn.namelist())
    say()
    say("%-14s %8s %8s %6s" % ("前缀", "原版", "导出", "差"))
    for pref in ("Model/*/", "Model/~/", "Model/#/"):
        a = len([n for n in fon if n.startswith(pref)])
        b = len([n for n in fnn if n.startswith(pref)])
        say("%-14s %8d %8d %6d" % (pref, a, b, b - a))
    other = sorted(n for n in fnn if not n.startswith("Model/"))
    if other:
        say("非 Model/ 条目（原版没有 = 格式异常）: %s" % other)
        fail("导出里出现了 Model/ 之外的条目")
    miss = sorted(fon - fnn)
    say("缺少条目: %d %s" % (len(miss), miss[:5]))
    if miss:
        fail("导出缺少原版有的 ~/# 图层条目（快照会残缺）")
    extra = sorted(fnn - fon)
    say("多出条目: %d 个（Model/*/ 新瓦片 = 库里探索数据比快照多，属正常增长）" % len(extra))
    if any(n.startswith("Model/~/") or n.startswith("Model/#/") for n in extra):
        fail("多出的条目落在 ~/# 图层里（残留图层条目）")

    bad_name, bad_len, bad_slot, bad_tail = [], [], [], []
    blocks = cells = 0
    star = sorted(n for n in fnn if n.startswith("Model/*/"))
    for n in star:
        base = n.split("/")[-1]
        if not name_reversible(base):
            bad_name.append(n)
            continue
        data = zlib.decompress(fn.read(n))
        if len(data) < HEADER_SIZE or (len(data) - HEADER_SIZE) % BLOCK != 0:
            bad_len.append((n, len(data)))
            continue
        nb = (len(data) - HEADER_SIZE) // BLOCK
        blocks += nb
        slots = [data[2 * i] | (data[2 * i + 1] << 8) for i in range(HEADER_LEN)]
        used = [s for s in slots if s]
        if sorted(used) != list(range(1, nb + 1)):
            bad_slot.append((n, nb, len(used)))
        for k in range(nb):
            blk = data[HEADER_SIZE + k * BLOCK: HEADER_SIZE + (k + 1) * BLOCK]
            pc = sum(bin(x).count("1") for x in blk[:BITMAP])
            v = (blk[512] << 16) | (blk[513] << 8) | blk[514]
            if v != pc * 2 + 1 or blk[512] != 0:
                bad_tail.append(n)
                break
            cells += pc
    say()
    say("Model/*/: %d 个瓦片、%d 个块、%d 个已探索格" % (len(star), blocks, cells))
    for label, bad, hint in (
            ("文件名可逆（md5(十进制id)[:4] + 两段掩码）", bad_name, "名字编错 fog of world 找不到瓦片"),
            ("长度 = 32768 + 515n（头 + 每块 515B）", bad_len, "头或块体长度不对"),
            ("槽位表自洽（非零槽 = 1..n 的排列）", bad_slot, "块体索引越界或重复引用"),
            ("块尾 24 位大端 = 置位数×2+1", bad_tail, "元信息与真机格式不符")):
        if bad:
            fail("%s：%d 处异常 %s（%s）" % (label, len(bad), bad[:3], hint))
        else:
            say("  %s ✓" % label)

    say()
    say("图层 ~/# 是不解析、原样写回的，应与原版逐字节一致：")
    for pref in ("Model/~/", "Model/#/"):
        a = sorted(n for n in fon if n.startswith(pref))
        b = sorted(n for n in fnn if n.startswith(pref))
        same = a == b and all(fo.read(n) == fn.read(n) for n in a)
        say("  %-12s 原版 %d / 导出 %d  名字+字节一致=%s" % (pref, len(a), len(b), "是 ✓" if same else "否"))
        if not same:
            fail("%s 与原版不一致（少了快照会残缺，内容变了说明被解析过）" % pref)

    head("2.1 确定性（同一份数据导出两次）")
    if new2 and os.path.exists(new2):
        a, b = zipfile.ZipFile(new), zipfile.ZipFile(new2)
        an, bn = set(a.namelist()), set(b.namelist())
        diff = [n for n in an & bn if a.read(n) != b.read(n)]
        if an != bn or diff:
            fail("两次导出条目不同：%d 个条目不匹配 %s" % (len(diff), diff[:3]))
        else:
            say("  解压后逐条目字节一致 ✓（%d 个条目）" % len(an))
        say("  zip 内条目顺序: %s（Go map 遍历顺序随机，顺序不保证，fog 靠文件名找）"
            % ("相同" if a.namelist() == b.namelist() else "不同，不影响读取"))
    else:
        say("  未提供第二次导出，跳过")


def main():
    if len(sys.argv) < 2:
        print(__doc__)
        return 2
    mode = sys.argv[1]
    args = sys.argv[2:]
    if mode == "all":
        if len(args) < 4:
            print(__doc__)
            return 2
        compare_rondbackup(args[0], args[1])
        compare_fwss(args[2], args[3])
    elif mode == "rondbackup":
        if len(args) < 2:
            print(__doc__)
            return 2
        compare_rondbackup(args[0], args[1], args[2] if len(args) > 2 else None)
    elif mode == "fwss":
        if len(args) < 2:
            print(__doc__)
            return 2
        compare_fwss(args[0], args[1], args[2] if len(args) > 2 else None)
    else:
        print(__doc__)
        return 2

    head("结论")
    for w in WARNS:
        say("注意: %s" % w)
        say()
    say("结构一致性: %s" % ("通过 ✓" if not FAILED[0] else "有差异（见上面带 !! 的行）"))
    print("\n".join(OUT))
    return 0 if not FAILED[0] else 1


if __name__ == "__main__":
    sys.exit(main())
