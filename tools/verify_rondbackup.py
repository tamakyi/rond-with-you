import zipfile, sqlite3, os, shutil, sys, tempfile

A = sys.argv[1] if len(sys.argv) > 1 else r"C:/OtherFiles/Github/rond-with-you/data/20260914_151649.rondbackup"
B = sys.argv[2] if len(sys.argv) > 2 else r"C:/OtherFiles/Github/rond-with-you/storage/export-final.rondbackup"

MANAGED = ["ZLOCATION", "ZVISIT", "ZMOVEMENT", "ZHOURLYWEATHER", "ZACTIVITY", "ZTAG", "ZTAGGROUP", "ZTRANSPORT"]
UNMANAGED = ["ZRAWVISIT", "ZKEYWORD", "ZPOICATEGORY", "ZACTIVITYGROUP"]

tmp = tempfile.mkdtemp(prefix="rondcmp")


def extract(path, tag):
    d = os.path.join(tmp, tag)
    os.makedirs(d, exist_ok=True)
    with zipfile.ZipFile(path) as z:
        z.extractall(d)
    for f in os.listdir(d):
        if f.endswith(".sqlite"):
            return os.path.join(d, f)
    for root, dirs, files in os.walk(d):
        for f in files:
            if f.endswith(".sqlite"):
                return os.path.join(root, f)
    raise SystemExit("no sqlite in " + path)


sa = extract(A, "orig")
sb = extract(B, "gen")

ca = sqlite3.connect(sa)
cb = sqlite3.connect(sb)

diff_total = 0


def rows(c, table):
    cur = c.execute('SELECT * FROM "%s"' % table)
    cols = [d[0] for d in cur.description]
    out = {}
    for r in cur.fetchall():
        out[r[0]] = dict(zip(cols, r))
    return out


def norm(v):
    if isinstance(v, float):
        return round(v, 6)
    if isinstance(v, bytes):
        return v
    return v


def cmp(table):
    global diff_total
    ra = rows(ca, table)
    rb = rows(cb, table)
    ca_ = sqlite3.connect(sa).execute('PRAGMA table_info("%s")' % table).fetchall()
    cols = [x[1] for x in ca_]
    miss = [k for k in ra if k not in rb]
    extra = [k for k in rb if k not in ra]
    vdiff = []
    for k in ra:
        if k not in rb:
            continue
        d = []
        for c in cols:
            va, vb = norm(ra[k].get(c)), norm(rb[k].get(c))
            if isinstance(va, bytes) or isinstance(vb, bytes):
                if va != vb:
                    d.append((c, type(va).__name__, type(vb).__name__))
                continue
            if va != vb:
                d.append((c, va, vb))
        if d:
            vdiff.append((k, d))
    n = len(miss) + len(extra) + len(vdiff)
    diff_total += n
    print("[%s] rows orig=%d gen=%d  missing=%d extra=%d valuediff=%d" % (table, len(ra), len(rb), len(miss), len(extra), len(vdiff)))
    if miss:
        print("   missing PKs:", miss[:10])
    if extra:
        print("   extra PKs:", extra[:10])
    for k, d in vdiff[:3]:
        print("   PK %s differs: %s" % (k, d[:8]))


print("=== MANAGED ===")
for t in MANAGED:
    cmp(t)
print("=== UNMANAGED ===")
for t in UNMANAGED:
    cmp(t)

# 结构对比
def struct(c):
    t = sorted(x[0] for x in c.execute("SELECT name FROM sqlite_master WHERE type='table'").fetchall())
    i = sorted(x[0] for x in c.execute("SELECT name FROM sqlite_master WHERE type='index'").fetchall())
    return t, i

ta, ia = struct(ca)
tb, ib = struct(cb)
print("tables: %d vs %d  equal=%s" % (len(ta), len(tb), ta == tb))
if ta != tb:
    print("  only-orig:", [x for x in ta if x not in tb])
    print("  only-gen :", [x for x in tb if x not in ta])
print("indexes: %d vs %d  equal=%s" % (len(ia), len(ib), ia == ib))
if ia != ib:
    print("  only-orig:", [x for x in ia if x not in ib])
    print("  only-gen :", [x for x in ib if x not in ia])

ma = ca.execute("SELECT Z_PLIST FROM Z_METADATA").fetchone()[0]
mb = cb.execute("SELECT Z_PLIST FROM Z_METADATA").fetchone()[0]
print("Z_METADATA plist: %d vs %d bytes equal=%s" % (len(ma), len(mb), ma == mb))

print("总差异 =", diff_total)
shutil.rmtree(tmp, ignore_errors=True)
