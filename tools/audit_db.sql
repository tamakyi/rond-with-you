-- rond-with-you 数据一致性审计（PostgreSQL）
-- 用法：psql "<dsn>" -f tools/audit_db.sql
-- 各项期望值均为 0；非 0 说明有漂移或悬空引用，逐项排查。
-- 注意：Windows + Git Bash 下 psql 不能直接吃中文命令行参数（会被按 GBK 传出去），
-- 所以这个文件必须保存为 UTF-8 并用 -f 执行。
\pset border 1

\echo '=== 1. 聚合统计漂移（应为 0） ==='
SELECT 'place.visit_count drift' AS chk, count(*) AS n
  FROM places p
 WHERE COALESCE(p.visit_count,0) <> (SELECT count(*) FROM visits v WHERE v.place_id=p.id)
UNION ALL
SELECT 'datasets.visit_count drift', count(*)
  FROM datasets d
 WHERE COALESCE(d.visit_count,0) <> (SELECT count(*) FROM visits v WHERE v.dataset_id=d.id)
UNION ALL
SELECT 'datasets.place_count drift', count(*)
  FROM datasets d
 WHERE COALESCE(d.place_count,0) <> (SELECT count(*) FROM places p WHERE p.dataset_id=d.id)
UNION ALL
SELECT 'duration_min wrong', count(*)
  FROM visits WHERE departure IS NOT NULL
   AND COALESCE(duration_min,-1) <> GREATEST(0, floor(extract(epoch FROM (departure-arrival))/60)::int)
UNION ALL
SELECT 'departure < arrival', count(*)
  FROM visits WHERE departure IS NOT NULL AND departure < arrival;

\echo '=== 2. 悬空引用（应为 0） ==='
SELECT 'visits -> other dataset place' AS chk, count(*)
  FROM visits v JOIN places p ON p.id=v.place_id WHERE p.dataset_id<>v.dataset_id
UNION ALL
SELECT 'movements.from_place', count(*)
  FROM movements m WHERE m.from_place_id IS NOT NULL
   AND NOT EXISTS (SELECT 1 FROM places p WHERE p.id=m.from_place_id)
UNION ALL
SELECT 'movements.to_place', count(*)
  FROM movements m WHERE m.to_place_id IS NOT NULL
   AND NOT EXISTS (SELECT 1 FROM places p WHERE p.id=m.to_place_id)
UNION ALL
SELECT 'movements.from_visit_src', count(*)
  FROM movements m WHERE m.from_visit_src IS NOT NULL
   AND NOT EXISTS (SELECT 1 FROM visits v WHERE v.dataset_id=m.dataset_id AND v.src_pk=m.from_visit_src)
UNION ALL
SELECT 'movements.to_visit_src', count(*)
  FROM movements m WHERE m.to_visit_src IS NOT NULL
   AND NOT EXISTS (SELECT 1 FROM visits v WHERE v.dataset_id=m.dataset_id AND v.src_pk=m.to_visit_src)
UNION ALL
SELECT 'weather.visit_src', count(*)
  FROM weather w WHERE w.visit_src IS NOT NULL
   AND NOT EXISTS (SELECT 1 FROM visits v WHERE v.dataset_id=w.dataset_id AND v.src_pk=w.visit_src)
UNION ALL
SELECT 'visit_tags dangling', count(*)
  FROM visit_tags vt
 WHERE NOT EXISTS (SELECT 1 FROM visits v WHERE v.id=vt.visit_id)
    OR NOT EXISTS (SELECT 1 FROM tags t WHERE t.id=vt.tag_id)
UNION ALL
SELECT 'visit_tags cross-dataset', count(*)
  FROM visit_tags vt
  JOIN visits v ON v.id=vt.visit_id JOIN tags t ON t.id=vt.tag_id
 WHERE v.dataset_id<>t.dataset_id
UNION ALL
SELECT 'movements end<start', count(*)
  FROM movements WHERE started_at IS NOT NULL AND ended_at IS NOT NULL AND ended_at < started_at;

\echo '=== 3. 留档与现役表的差集（skipped 行属正常，见下方明细） ==='
SELECT r.entity, count(*) FILTER (WHERE r.skipped) AS skipped_ok, count(*) FILTER (WHERE NOT r.skipped) AS should_be_zero
  FROM entity_raw r
 WHERE (r.entity='ZLOCATION'      AND NOT EXISTS (SELECT 1 FROM places p     WHERE p.dataset_id=r.dataset_id AND p.src_pk=r.src_pk))
    OR (r.entity='ZVISIT'         AND NOT EXISTS (SELECT 1 FROM visits v     WHERE v.dataset_id=r.dataset_id AND v.src_pk=r.src_pk))
    OR (r.entity='ZMOVEMENT'      AND NOT EXISTS (SELECT 1 FROM movements m  WHERE m.dataset_id=r.dataset_id AND m.src_pk=r.src_pk))
    OR (r.entity='ZHOURLYWEATHER' AND NOT EXISTS (SELECT 1 FROM weather w    WHERE w.dataset_id=r.dataset_id AND w.src_pk=r.src_pk))
    OR (r.entity='ZACTIVITY'      AND NOT EXISTS (SELECT 1 FROM activities a WHERE a.dataset_id=r.dataset_id AND a.src_pk=r.src_pk))
    OR (r.entity='ZTAG'           AND NOT EXISTS (SELECT 1 FROM tags t       WHERE t.dataset_id=r.dataset_id AND t.src_pk=r.src_pk))
 GROUP BY r.entity;

\echo '=== 4. 笔记本/投票按 src_pk 关联，是否落在当前数据集（>0 即跨数据集错位） ==='
SELECT 'notes not in active dataset' AS chk, count(*) FROM place_notes n
 WHERE NOT EXISTS (SELECT 1 FROM places p WHERE p.dataset_id=(SELECT id FROM datasets WHERE is_active ORDER BY id DESC LIMIT 1) AND p.src_pk=n.src_pk)
UNION ALL
SELECT 'votes not in active dataset', count(*)
  FROM place_votes v
 WHERE NOT EXISTS (SELECT 1 FROM places p WHERE p.dataset_id=(SELECT id FROM datasets WHERE is_active ORDER BY id DESC LIMIT 1) AND p.src_pk=v.src_pk);

\echo '=== 5. 数据特征（非缺陷，仅供了解） ==='
SELECT 'places 同名同坐标重复组数' AS item, count(*)::text AS v FROM (
  SELECT 1 FROM places GROUP BY name, round(lat::numeric,5), round(lon::numeric,5) HAVING count(*)>1) t
UNION ALL
SELECT 'visits 零时长', count(*)::text FROM visits WHERE duration_min=0
UNION ALL
SELECT 'visits 超 30 天', count(*)::text FROM visits WHERE duration_min>43200
UNION ALL
SELECT 'visits 无活动', count(*)::text FROM visits WHERE activity_id IS NULL
UNION ALL
SELECT 'places 无类别', count(*)::text FROM places WHERE COALESCE(poi_category,'')=''
UNION ALL
SELECT 'places 无坐标', count(*)::text FROM places WHERE COALESCE(lat,0)=0 AND COALESCE(lon,0)=0;
