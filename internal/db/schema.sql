-- rond-with-you 数据结构
-- 一次上传 = 一个 dataset，所有事实表挂在其下，支持多份备份共存与切换

CREATE TABLE IF NOT EXISTS users (
    id            BIGSERIAL PRIMARY KEY,
    username      TEXT NOT NULL UNIQUE,
    password_hash TEXT NOT NULL,
    display_name  TEXT,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_login_at TIMESTAMPTZ
);

CREATE TABLE IF NOT EXISTS datasets (
    id                BIGSERIAL PRIMARY KEY,
    user_id           BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    original_name     TEXT NOT NULL,
    sha256            CHAR(64) NOT NULL,
    size_bytes        BIGINT NOT NULL DEFAULT 0,
    uploaded_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    source_created_at TIMESTAMPTZ,
    device_name       TEXT,
    is_active         BOOLEAN NOT NULL DEFAULT FALSE,
    status            TEXT NOT NULL DEFAULT 'done',
    error             TEXT,
    visit_count       INT NOT NULL DEFAULT 0,
    place_count       INT NOT NULL DEFAULT 0,
    raw_visit_count   INT NOT NULL DEFAULT 0,
    first_visit_at    TIMESTAMPTZ,
    last_visit_at     TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS idx_datasets_user ON datasets(user_id, uploaded_at DESC);
CREATE UNIQUE INDEX IF NOT EXISTS idx_datasets_sha ON datasets(user_id, sha256);

CREATE TABLE IF NOT EXISTS activities (
    id          BIGSERIAL PRIMARY KEY,
    dataset_id  BIGINT NOT NULL REFERENCES datasets(id) ON DELETE CASCADE,
    src_pk      INT NOT NULL,
    name        TEXT NOT NULL,
    color       TEXT,
    icon        TEXT,
    is_home     BOOLEAN NOT NULL DEFAULT FALSE,
    is_work     BOOLEAN NOT NULL DEFAULT FALSE,
    is_excluded BOOLEAN NOT NULL DEFAULT FALSE,
    is_archived BOOLEAN NOT NULL DEFAULT FALSE,
    UNIQUE (dataset_id, src_pk)
);

CREATE TABLE IF NOT EXISTS tags (
    id         BIGSERIAL PRIMARY KEY,
    dataset_id BIGINT NOT NULL REFERENCES datasets(id) ON DELETE CASCADE,
    src_pk     INT NOT NULL,
    name       TEXT NOT NULL,
    color      TEXT,
    group_name TEXT,
    UNIQUE (dataset_id, src_pk)
);

CREATE TABLE IF NOT EXISTS places (
    id             BIGSERIAL PRIMARY KEY,
    dataset_id     BIGINT NOT NULL REFERENCES datasets(id) ON DELETE CASCADE,
    src_pk         INT NOT NULL,
    name           TEXT,
    poi_category   TEXT,
    lat            DOUBLE PRECISION,
    lon            DOUBLE PRECISION,
    country_code   TEXT,
    province       TEXT,
    city           TEXT,
    district       TEXT,
    sublocality    TEXT,
    thoroughfare   TEXT,
    timezone       TEXT,
    visit_count    INT NOT NULL DEFAULT 0,
    dwell_minutes  BIGINT NOT NULL DEFAULT 0,
    first_visit_at TIMESTAMPTZ,
    last_visit_at  TIMESTAMPTZ,
    UNIQUE (dataset_id, src_pk)
);
CREATE INDEX IF NOT EXISTS idx_places_dataset ON places(dataset_id);
CREATE INDEX IF NOT EXISTS idx_places_city ON places(dataset_id, city);
CREATE INDEX IF NOT EXISTS idx_places_geo ON places(dataset_id, lat, lon);

CREATE TABLE IF NOT EXISTS visits (
    id             BIGSERIAL PRIMARY KEY,
    dataset_id     BIGINT NOT NULL REFERENCES datasets(id) ON DELETE CASCADE,
    src_pk         INT NOT NULL,
    place_id       BIGINT REFERENCES places(id) ON DELETE CASCADE,
    activity_id    BIGINT REFERENCES activities(id) ON DELETE SET NULL,
    arrival        TIMESTAMPTZ NOT NULL,
    departure      TIMESTAMPTZ,
    duration_min   INT,
    is_home        BOOLEAN NOT NULL DEFAULT FALSE,
    is_work        BOOLEAN NOT NULL DEFAULT FALSE,
    bookmarked     BOOLEAN NOT NULL DEFAULT FALSE,
    is_user_added  BOOLEAN NOT NULL DEFAULT FALSE,
    remark         TEXT,
    emoji          TEXT,
    weather_symbol TEXT,
    UNIQUE (dataset_id, src_pk)
);
CREATE INDEX IF NOT EXISTS idx_visits_arrival ON visits(dataset_id, arrival DESC);
CREATE INDEX IF NOT EXISTS idx_visits_place ON visits(dataset_id, place_id);
CREATE INDEX IF NOT EXISTS idx_visits_activity ON visits(dataset_id, activity_id);

CREATE TABLE IF NOT EXISTS visit_tags (
    visit_id BIGINT NOT NULL REFERENCES visits(id) ON DELETE CASCADE,
    tag_id   BIGINT NOT NULL REFERENCES tags(id) ON DELETE CASCADE,
    PRIMARY KEY (visit_id, tag_id)
);

CREATE TABLE IF NOT EXISTS movements (
    id              BIGSERIAL PRIMARY KEY,
    dataset_id      BIGINT NOT NULL REFERENCES datasets(id) ON DELETE CASCADE,
    user_id         BIGINT,
    src_pk          INT NOT NULL,
    transport_src   INT,
    transport_name  TEXT,
    transport_color TEXT,
    transport_icon  TEXT,
    from_visit_src  INT,
    to_visit_src    INT,
    from_place_id   BIGINT REFERENCES places(id) ON DELETE SET NULL,
    to_place_id     BIGINT REFERENCES places(id) ON DELETE SET NULL,
    started_at      TIMESTAMPTZ,
    ended_at        TIMESTAMPTZ,
    duration_min    INT,
    distance_km     DOUBLE PRECISION,
    UNIQUE (dataset_id, src_pk)
);
CREATE INDEX IF NOT EXISTS idx_movements_start ON movements(dataset_id, started_at DESC);

CREATE TABLE IF NOT EXISTS weather (
    id                   BIGSERIAL PRIMARY KEY,
    dataset_id           BIGINT NOT NULL REFERENCES datasets(id) ON DELETE CASCADE,
    user_id              BIGINT,
    src_pk               INT NOT NULL,
    visit_src            INT,
    at                   TIMESTAMPTZ,
    is_daylight          BOOLEAN,
    temperature_c        DOUBLE PRECISION,
    apparent_c           DOUBLE PRECISION,
    humidity             DOUBLE PRECISION,
    precipitation_amount DOUBLE PRECISION,
    precipitation_chance DOUBLE PRECISION,
    wind_speed           DOUBLE PRECISION,
    visibility           DOUBLE PRECISION,
    uv_index             INT,
    condition            TEXT,
    symbol               TEXT,
    uv_category          TEXT,
    wind_direction       TEXT,
    UNIQUE (dataset_id, src_pk)
);
CREATE INDEX IF NOT EXISTS idx_weather_at ON weather(dataset_id, at);

CREATE TABLE IF NOT EXISTS settings (
    key        TEXT PRIMARY KEY,
    value      JSONB NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- 世界迷雾（Fog of World）已探索位图，按块粒度存储。
-- 迷雾与数据集无关（来自另一个应用），挂在用户维度下；
-- gbx/gby 为全球块坐标（0..65535），一个位对应 2^22 网格里约 9.55m 的一格。
CREATE TABLE IF NOT EXISTS fog_blocks (
    user_id    BIGINT NOT NULL,
    gbx        INT NOT NULL,
    gby        INT NOT NULL,
    bitmap     BYTEA NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, gbx, gby)
);

-- 地点备注与别名。按 src_pk（rond 内部地点 ID，跨快照稳定）而不是 places.id 存，
-- 这样重新上传备份、数据集换了新 id 之后备注依然挂着。
-- dataset_id 是后加的：src_pk 只在单个数据集内唯一，缺了它切数据集时同编号的
-- 地点会把别人的备注/别名认领过去。
CREATE TABLE IF NOT EXISTS place_notes (
    user_id    BIGINT NOT NULL,
    dataset_id BIGINT NOT NULL DEFAULT 0,
    src_pk     INT NOT NULL,
    alias      TEXT NOT NULL DEFAULT '',
    note       TEXT NOT NULL DEFAULT '',
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, dataset_id, src_pk)
);

-- 站长对地点的「官方结论」：推荐(1) / 踩雷(-1)，与自由文本备注分开存，同样按 src_pk。
CREATE TABLE IF NOT EXISTS place_votes (
    user_id    BIGINT NOT NULL,
    dataset_id BIGINT NOT NULL DEFAULT 0,
    src_pk     INT NOT NULL,
    verdict    SMALLINT NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, dataset_id, src_pk)
);

-- 访客对地点的投票：一人一票、可改可撤。voter_key 由匿名 cookie + IP 派生，
-- 与数据集无关（换快照不丢），故不挂 dataset_id。
CREATE TABLE IF NOT EXISTS place_visitor_votes (
    src_pk     INT NOT NULL,
    voter_key  TEXT NOT NULL,
    vote       SMALLINT NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (src_pk, voter_key)
);

CREATE INDEX IF NOT EXISTS idx_pvv_src ON place_visitor_votes(src_pk);

-- GPX 轨迹：rond 只记录「地点到地点」的直线位移，导入 GPX 可以把真实路径补上。
-- 坐标统一按 WGS-84 存（GPX 标准），展示时再按底图坐标系转换。
CREATE TABLE IF NOT EXISTS gpx_tracks (
    id          BIGSERIAL PRIMARY KEY,
    user_id     BIGINT NOT NULL,
    name        TEXT NOT NULL,
    point_count INT NOT NULL DEFAULT 0,
    length_km   DOUBLE PRECISION NOT NULL DEFAULT 0,
    started_at  TIMESTAMPTZ,
    points      JSONB NOT NULL,
    uploaded_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_gpx_user ON gpx_tracks(user_id, uploaded_at DESC);

-- ---------- 增量迁移 ----------
-- 数据集是「快照」语义：一次上传 = 一份完整数据集，页面始终只展示其中一份。
-- 因此判重不做行级去重，而是「文件摘要 + 内容指纹」两道，见 datasets.content_hash。
-- 这里只给事实表补上 user_id（分区键，也为将来多用户留位）。
ALTER TABLE visits    ADD COLUMN IF NOT EXISTS user_id BIGINT;
ALTER TABLE movements ADD COLUMN IF NOT EXISTS user_id BIGINT;
ALTER TABLE weather   ADD COLUMN IF NOT EXISTS user_id BIGINT;

-- 回填失败绝不能拖垮启动：老库若已存在重复行，唯一索引会让回填直接报错。
-- 这里逐条兜住，最坏情况只是这些历史行保持 NULL，站点照常启动。
DO $mig$
BEGIN
    BEGIN
        UPDATE visits SET user_id = d.user_id FROM datasets d
        WHERE d.id = visits.dataset_id AND visits.user_id IS NULL;
    EXCEPTION WHEN unique_violation THEN
        RAISE NOTICE 'visits 回填 user_id 遇到重复行，已跳过';
    END;
    BEGIN
        UPDATE movements SET user_id = d.user_id FROM datasets d
        WHERE d.id = movements.dataset_id AND movements.user_id IS NULL;
    EXCEPTION WHEN unique_violation THEN
        RAISE NOTICE 'movements 回填 user_id 遇到重复行，已跳过';
    END;
    BEGIN
        UPDATE weather SET user_id = d.user_id FROM datasets d
        WHERE d.id = weather.dataset_id AND weather.user_id IS NULL;
    EXCEPTION WHEN unique_violation THEN
        RAISE NOTICE 'weather 回填 user_id 遇到重复行，已跳过';
    END;
END
$mig$;

-- 早期版本把 (user_id, src_pk) 做成了跨数据集全局唯一，想实现「重复导入不堆积」。
-- 但那会让更新备份里的新记录只落进未激活的数据集（页面上看不到），已废弃，这里清理掉。
-- 行级唯一性仍由各表自带的 UNIQUE (dataset_id, src_pk) 保证。
DROP INDEX IF EXISTS idx_visits_dedupe;
DROP INDEX IF EXISTS idx_movements_dedupe;
DROP INDEX IF EXISTS idx_weather_dedupe;

-- 内容指纹：识别「内容相同、但文件字节不同」的重复上传（rond 每次重新导出字节都会变）
ALTER TABLE datasets ADD COLUMN IF NOT EXISTS content_hash TEXT;

-- 管理员 TOTP 两步验证：secret 为 base32（无填充）；enabled=false 时 secret 仅作为
-- 「已生成待确认」状态存在，确认验证码通过后才允许登录启用
ALTER TABLE users ADD COLUMN IF NOT EXISTS totp_secret  TEXT;
ALTER TABLE users ADD COLUMN IF NOT EXISTS totp_enabled BOOLEAN NOT NULL DEFAULT FALSE;
CREATE INDEX IF NOT EXISTS idx_datasets_content ON datasets(user_id, content_hash);

-- rond 备份里按次到访记录的备注（ZVISIT.ZREMARK_）汇总后的缓存。
-- 单独一列存：回填只重写这一列，站长手写的 note 永远不受影响；
-- 展示时把 rond_note 拼在 note 之前（见 web.applyNotes 的 joinNote）。
ALTER TABLE place_notes ADD COLUMN IF NOT EXISTS rond_note TEXT NOT NULL DEFAULT '';

-- 导出 rondbackup 所需的 Core Data 元信息（实体编号、ZPRIMARYKEY、建表 DDL、访问↔标签关联表），
-- 导入时抓取并缓存，导出时据此从 PostgreSQL 重建 LifeEasy.sqlite。NULL 表示旧数据集未抓到，
-- 需要把原始 .rondbackup 重新上传一次（即便内容相同也会补抓）。
ALTER TABLE datasets ADD COLUMN IF NOT EXISTS coredata_meta JSONB;

-- rond 原始行留档：每条实体在 Core Data 里的**完整一行**（含站点未映射的字段）
-- 按 JSONB 存下来。有了它，导出不再依赖基线库去补那些字段——
-- 基线即便丢了也能精确还原，站点侧也具备完整数据，不会因为映射范围窄而漏字段。
-- BLOB 编码为 {"$blob": "<base64>"}；时间戳保持 Core Data 的浮点秒，避免精度损失。
CREATE TABLE IF NOT EXISTS entity_raw (
    dataset_id BIGINT   NOT NULL REFERENCES datasets(id) ON DELETE CASCADE,
    entity     TEXT     NOT NULL,
    src_pk     INT      NOT NULL,
    raw        JSONB    NOT NULL,
    -- skipped=真 表示这行因缺少必填字段没能进 PostgreSQL（例如没有到达时间的孤独到访）。
    -- 导出时这些行要按留档原样写回，否则往返一趟就少数据；而用户在站点上删掉的
    -- 行不在此列，不会被「复活」。
    skipped    BOOLEAN  NOT NULL DEFAULT FALSE,
    PRIMARY KEY (dataset_id, entity, src_pk)
);
CREATE INDEX IF NOT EXISTS idx_entity_raw_lookup ON entity_raw (dataset_id, entity);
-- 旧库升级：补上后加的 skipped 列
ALTER TABLE entity_raw ADD COLUMN IF NOT EXISTS skipped BOOLEAN NOT NULL DEFAULT FALSE;

-- 旧库升级：place_notes / place_votes 补 dataset_id 并把主键扩成三列。
-- 回填归到当前激活数据集——旧数据只能这么认领，之后按数据集各管各的。
ALTER TABLE place_notes ADD COLUMN IF NOT EXISTS dataset_id BIGINT NOT NULL DEFAULT 0;
ALTER TABLE place_votes ADD COLUMN IF NOT EXISTS dataset_id BIGINT NOT NULL DEFAULT 0;
UPDATE place_notes SET dataset_id = COALESCE((SELECT id FROM datasets WHERE is_active ORDER BY id DESC LIMIT 1), 0)
 WHERE dataset_id = 0;
UPDATE place_votes SET dataset_id = COALESCE((SELECT id FROM datasets WHERE is_active ORDER BY id DESC LIMIT 1), 0)
 WHERE dataset_id = 0;

DO $$
DECLARE cols text;
BEGIN
    SELECT string_agg(a.attname, ',' ORDER BY array_position(c.conkey, a.attnum))
      INTO cols
      FROM pg_constraint c
      JOIN pg_attribute a ON a.attrelid = c.conrelid AND a.attnum = ANY(c.conkey)
     WHERE c.conname = 'place_notes_pkey';
    IF cols IS DISTINCT FROM 'user_id,dataset_id,src_pk' THEN
        ALTER TABLE place_notes DROP CONSTRAINT IF EXISTS place_notes_pkey;
        ALTER TABLE place_notes ADD CONSTRAINT place_notes_pkey PRIMARY KEY (user_id, dataset_id, src_pk);
    END IF;

    SELECT string_agg(a.attname, ',' ORDER BY array_position(c.conkey, a.attnum))
      INTO cols
      FROM pg_constraint c
      JOIN pg_attribute a ON a.attrelid = c.conrelid AND a.attnum = ANY(c.conkey)
     WHERE c.conname = 'place_votes_pkey';
    IF cols IS DISTINCT FROM 'user_id,dataset_id,src_pk' THEN
        ALTER TABLE place_votes DROP CONSTRAINT IF EXISTS place_votes_pkey;
        ALTER TABLE place_votes ADD CONSTRAINT place_votes_pkey PRIMARY KEY (user_id, dataset_id, src_pk);
    END IF;
END $$;

-- 专题：把某段时间内、某个区域里的足迹（地点 + 迷雾）打包成一个可对外展示的页面。
-- 区域用「中心点 + 半径」，半径为 0 表示不限区域（时间范围内的地点画到哪算哪）。
CREATE TABLE IF NOT EXISTS topics (
    id          BIGSERIAL PRIMARY KEY,
    user_id     BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    dataset_id  BIGINT NOT NULL REFERENCES datasets(id) ON DELETE CASCADE,
    title       TEXT NOT NULL,
    subtitle    TEXT NOT NULL DEFAULT '',
    description TEXT NOT NULL DEFAULT '',
    start_at    TIMESTAMPTZ NOT NULL,
    end_at      TIMESTAMPTZ NOT NULL,
    center_lat  DOUBLE PRECISION NOT NULL DEFAULT 0,
    center_lon  DOUBLE PRECISION NOT NULL DEFAULT 0,
    radius_km   DOUBLE PRECISION NOT NULL DEFAULT 0,
    show_fog    BOOLEAN NOT NULL DEFAULT TRUE,
    -- enabled=假 时前台不可见（列表不出现、详情 404），站长仍可在后台预览
    enabled     BOOLEAN NOT NULL DEFAULT TRUE,
    sort_order  INT NOT NULL DEFAULT 0,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_topics_user ON topics(user_id, sort_order, id);

-- fog of world 快照里「非已探索层」的原始瓦片文件（Model/~ 与 Model/#）。
-- 站点只用已探索层渲染，但导出必须原样写回这两层，否则重导出的快照会丢掉约 55% 的内容。
-- data 存的是原始（zlib 压缩后的）字节，一个字节都不动。
CREATE TABLE IF NOT EXISTS fog_raw_files (
    user_id  BIGINT NOT NULL,
    path     TEXT   NOT NULL,
    data     BYTEA  NOT NULL,
    PRIMARY KEY (user_id, path)
);

-- 迷雾遮罩区：这些圆形范围内的迷雾对访客隐藏（站长自己照常看得到）。
-- 存的中心是 **GCJ-02**（与 places 同系），渲染成瓦片前会换算到迷雾网格的 WGS-84。
CREATE TABLE IF NOT EXISTS fog_masks (
    id         BIGSERIAL PRIMARY KEY,
    user_id    BIGINT NOT NULL,
    name       TEXT   NOT NULL DEFAULT '',
    lat        DOUBLE PRECISION NOT NULL,
    lon        DOUBLE PRECISION NOT NULL,
    radius_m   DOUBLE PRECISION NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- 地点坐标的来源留档。以前没有任何记录，只能靠「留档里 ZRAWLATITUDE 与显示坐标是否相同」
-- 反推——照片导入的点没有 ZLOCATION 留档行，根本判不出来。2026-09 那次照片导入选错坐标系
-- （把 WGS-84 的 EXIF 当成已经是 GCJ-02）导致 63 个点整体偏 500~670m，事后只能逐张对原图
-- 才查出来，就是因为缺这几列。现在建点的那一刻就写下来：
--   coord_source 哪个入口建的：rond（导入备份）/ photo（照片导入）/ manual（后台新增地点）
--   coord_sys    提交上来的坐标本身属于哪个坐标系：wgs84 / gcj02
--   src_lat/lon  提交上来、**还没做任何换算**的原始坐标
-- 三者齐全时，显示坐标 = 按 coord_sys 换算 src_lat/lon 的结果，随时可复算、可核对。
-- 留空的含义是「历史行、没有留档」，不要在迁移里用启发式去猜（猜错比留空更糟）。
ALTER TABLE places ADD COLUMN IF NOT EXISTS coord_source TEXT;
ALTER TABLE places ADD COLUMN IF NOT EXISTS coord_sys    TEXT;
ALTER TABLE places ADD COLUMN IF NOT EXISTS src_lat      DOUBLE PRECISION;
ALTER TABLE places ADD COLUMN IF NOT EXISTS src_lon      DOUBLE PRECISION;

-- 存量回填不在这里做：判据需要 geo 包的 GCJ-02 往返换算（见 internal/ingest/coord.go），
-- SQL 里写不出来。启动时由 ingest.BackfillAllCoordSource 统一处理，那里同时能识别出
-- 「站点导出的备份给新建地点合成的 ZLOCATION 行」，不会把它误当成真机的原始 GPS。

-- 地点图片：站长给地点配的图。字节由**浏览器**转成 WebP 后再上传（服务端只负责收下与转存），
-- 1 核机器不用扛解码。storage 记这一张实际放在哪、object_key 是本地相对路径或对象存储 key：
-- 换了存储后端之后旧图仍按各自那一列去取，不会因为改了设置就全变破图。
CREATE TABLE IF NOT EXISTS place_images (
    id         BIGSERIAL PRIMARY KEY,
    dataset_id BIGINT NOT NULL REFERENCES datasets(id) ON DELETE CASCADE,
    src_pk     INT NOT NULL,
    storage    TEXT NOT NULL DEFAULT 'local',
    object_key TEXT NOT NULL,
    width      INT NOT NULL DEFAULT 0,
    height     INT NOT NULL DEFAULT 0,
    bytes      BIGINT NOT NULL DEFAULT 0,
    position   INT NOT NULL DEFAULT 0,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (dataset_id, src_pk, object_key)
);
CREATE INDEX IF NOT EXISTS idx_place_images_place ON place_images(dataset_id, src_pk, position);
