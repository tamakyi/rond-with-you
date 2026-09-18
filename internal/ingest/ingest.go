package ingest

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"time"

	"rond-with-you/internal/geo"
	"rond-with-you/internal/rond"
)

type Stats struct {
	Visits     int
	Places     int
	RawVisits  int
	Skipped    int
	Identical  bool // 内容与已有数据集完全一致，本次没有新建快照
	FirstVisit *time.Time
	LastVisit  *time.Time
}

// File 计算文件摘要，用于去重。
func File(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// insertEntityRaw 把「实体 -> src_pk -> 完整原始行」写进 entity_raw。
// 单条失败不阻断导入（留档只影响导出保真度），但会记日志提示。
func insertEntityRaw(ctx context.Context, tx *sql.Tx, datasetID int64, rows map[string]map[int]map[string]any, skipped map[string]map[int]bool) error {
	for entity, byPK := range rows {
		for pk, raw := range byPK {
			blob, err := json.Marshal(raw)
			if err != nil {
				log.Printf("留档 %s#%d 序列化失败: %v", entity, pk, err)
				continue
			}
			// 传 string 而不是 []byte：pgx 会把 []byte 当 bytea，jsonb 列要文本
			if _, err := tx.ExecContext(ctx, `INSERT INTO entity_raw (dataset_id, entity, src_pk, raw, skipped)
				VALUES ($1,$2,$3,$4,$5)
				ON CONFLICT (dataset_id, entity, src_pk) DO UPDATE SET raw=EXCLUDED.raw, skipped=EXCLUDED.skipped`,
				datasetID, entity, pk, string(blob), skipped[entity][pk]); err != nil {
				return fmt.Errorf("留档 %s 原始行: %w", entity, err)
			}
		}
	}
	return nil
}

// contentHash 给备份内容算指纹，用来识别「内容没变、只是重新导出了一次」的上传。
// rond 每次导出连 WAL 一起打包，字节必然不同，文件摘要挡不住这种重复；
// 但同一批到访的 Z_PK 是稳定的，所以按到访自然键排序后取摘要即可。
func contentHash(b *rond.Backup) string {
	keys := make([]string, 0, len(b.Visits))
	for _, v := range b.Visits {
		if v.Arrival == nil {
			continue
		}
		keys = append(keys, fmt.Sprintf("%d|%d", v.SrcPK, v.Arrival.Unix()))
	}
	sort.Strings(keys)
	h := sha256.New()
	for _, k := range keys {
		h.Write([]byte(k))
		h.Write([]byte{'\n'})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// Import 把解析后的备份写入 PostgreSQL，整个过程在一个事务内完成。
func Import(ctx context.Context, db *sql.DB, userID int64, originalName, sha string, size int64, b *rond.Backup) (int64, *Stats, error) {
	hash := contentHash(b)

	// 留档 Core Data 结构快照（实体编号 / DDL / 关联表）。失败不致命，留 NULL。
	metaJSON, _ := json.Marshal(b.Meta)
	hasMeta := len(metaJSON) > 0 && string(metaJSON) != "null"

	// 内容与已有数据集完全一致时直接复用，不新建快照，也不改动当前展示的那一份。
	var prevID int64
	var prev Stats
	err := db.QueryRowContext(ctx, `SELECT id, visit_count, place_count, raw_visit_count FROM datasets
		WHERE user_id=$1 AND content_hash=$2 AND status='done'
		ORDER BY uploaded_at DESC LIMIT 1`, userID, hash).
		Scan(&prevID, &prev.Visits, &prev.Places, &prev.RawVisits)
	switch {
	case err == nil:
		if hasMeta {
			if _, e := db.ExecContext(ctx, `UPDATE datasets SET coredata_meta=$1 WHERE id=$2`, metaJSON, prevID); e != nil {
				log.Printf("补抓 Core Data 元信息失败: %v", e)
			}
		}
		prev.Identical = true
		return prevID, &prev, nil
	case !errors.Is(err, sql.ErrNoRows):
		return 0, nil, err
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, nil, err
	}
	defer tx.Rollback()

	var datasetID int64
	err = tx.QueryRowContext(ctx, `INSERT INTO datasets (user_id, original_name, sha256, size_bytes, status)
		VALUES ($1,$2,$3,$4,'importing') RETURNING id`, userID, originalName, sha, size).Scan(&datasetID)
	if err != nil {
		return 0, nil, fmt.Errorf("创建数据集: %w", err)
	}

	// 缓存 Core Data 元信息，导出 rondbackup 时据此重建库结构。
	if hasMeta {
		if _, err := tx.ExecContext(ctx, `UPDATE datasets SET coredata_meta=$1 WHERE id=$2`, metaJSON, datasetID); err != nil {
			return 0, nil, fmt.Errorf("写入 Core Data 元信息: %w", err)
		}
	}

	// 站点模型装不下的行（缺必填字段）记在这里，导出时按留档原样写回。
	rawSkipped := map[string]map[int]bool{}
	skip := func(entity string, pk int) {
		if rawSkipped[entity] == nil {
			rawSkipped[entity] = map[int]bool{}
		}
		rawSkipped[entity][pk] = true
	}

	// 活动类型
	actID := map[int]int64{}
	homeAct, workAct := map[int]bool{}, map[int]bool{}
	for _, a := range b.Activities {
		var id int64
		if err := tx.QueryRowContext(ctx, `INSERT INTO activities
			(dataset_id, src_pk, name, color, icon, is_home, is_work, is_excluded, is_archived)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9) RETURNING id`,
			datasetID, a.SrcPK, a.Name, a.Color, a.Icon, a.IsHome, a.IsWork, a.IsExcluded, a.IsArchived).Scan(&id); err != nil {
			return 0, nil, fmt.Errorf("写入活动类型: %w", err)
		}
		actID[a.SrcPK] = id
		homeAct[a.SrcPK] = a.IsHome
		workAct[a.SrcPK] = a.IsWork
	}

	// 标签
	tagID := map[int]int64{}
	for _, t := range b.Tags {
		var id int64
		if err := tx.QueryRowContext(ctx, `INSERT INTO tags (dataset_id, src_pk, name, color, group_name)
			VALUES ($1,$2,$3,$4,NULLIF($5,'')) RETURNING id`,
			datasetID, t.SrcPK, t.Name, t.Color, t.GroupName).Scan(&id); err != nil {
			return 0, nil, fmt.Errorf("写入标签: %w", err)
		}
		tagID[t.SrcPK] = id
	}

	// 交通方式
	transName, transColor, transIcon := map[int]string{}, map[int]string{}, map[int]string{}
	for _, t := range b.Transports {
		transName[t.SrcPK], transColor[t.SrcPK], transIcon[t.SrcPK] = t.Name, t.Color, t.Icon
	}

	// 地点
	placeID := map[int]int64{}
	type locAgg struct {
		count int
		dwell int64
		first *time.Time
		last  *time.Time
	}
	agg := map[int]*locAgg{}
	locCoord := map[int][2]float64{}
	for _, l := range b.Locations {
		var id int64
		if err := tx.QueryRowContext(ctx, `INSERT INTO places
			(dataset_id, src_pk, name, poi_category, lat, lon, country_code, province, city,
			 district, sublocality, thoroughfare, timezone)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13) RETURNING id`,
			datasetID, l.SrcPK, l.Name, nullStr(l.POICategory), l.Lat, l.Lon,
			nullStr(l.CountryCode), nullStr(l.Province), nullStr(l.City), nullStr(l.District),
			nullStr(l.Sublocality), nullStr(l.Thoroughfare), nullStr(l.Timezone)).Scan(&id); err != nil {
			return 0, nil, fmt.Errorf("写入地点: %w", err)
		}
		placeID[l.SrcPK] = id
		agg[l.SrcPK] = &locAgg{}
		locCoord[l.SrcPK] = [2]float64{l.Lat, l.Lon}
	}

	// 访问记录
	visitID := map[int]int64{}
	var first, last *time.Time
	var vcount, skipped int
	visitPlace := map[int]int{}
	for _, v := range b.Visits {
		if v.Arrival == nil {
			skip("ZVISIT", v.SrcPK)
			continue
		}
		var pid any
		if v.LocationSrcPK != nil {
			if id, ok := placeID[*v.LocationSrcPK]; ok {
				pid = id
			}
		}
		var aid any
		isHome, isWork := false, false
		if v.ActivitySrcPK != nil {
			if id, ok := actID[*v.ActivitySrcPK]; ok {
				aid = id
			}
			isHome, isWork = homeAct[*v.ActivitySrcPK], workAct[*v.ActivitySrcPK]
		}
		var dur any
		if v.Departure != nil {
			m := int(v.Departure.Sub(*v.Arrival).Minutes())
			if m < 0 {
				m = 0
			}
			dur = m
		}
		var id int64
		err := tx.QueryRowContext(ctx, `INSERT INTO visits
			(dataset_id, user_id, src_pk, place_id, activity_id, arrival, departure, duration_min,
			 is_home, is_work, bookmarked, is_user_added, remark, emoji, weather_symbol)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,NULLIF($13,''),NULLIF($14,''),NULLIF($15,''))
			ON CONFLICT DO NOTHING
			RETURNING id`,
			datasetID, userID, v.SrcPK, pid, aid, v.Arrival, v.Departure, dur,
			isHome, isWork, v.Bookmarked, v.UserAdded, v.Remark, v.Emoji, v.WeatherSymbol).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			// 该到访已随更早的备份导入过，跳过即可，连同它的标签一起跳过
			skipped++
			continue
		}
		if err != nil {
			return 0, nil, fmt.Errorf("写入访问记录: %w", err)
		}
		visitID[v.SrcPK] = id
		vcount++

		if v.LocationSrcPK != nil {
			if a, ok := agg[*v.LocationSrcPK]; ok {
				a.count++
				if dur != nil {
					a.dwell += int64(dur.(int))
				}
				if a.first == nil || v.Arrival.Before(*a.first) {
					a.first = v.Arrival
				}
				if a.last == nil || v.Arrival.After(*a.last) {
					a.last = v.Arrival
				}
			}
			visitPlace[v.SrcPK] = *v.LocationSrcPK
		}
		if first == nil || v.Arrival.Before(*first) {
			first = v.Arrival
		}
		if last == nil || v.Arrival.After(*last) {
			last = v.Arrival
		}

		for _, tpk := range v.TagSrcPKs {
			if tid, ok := tagID[tpk]; ok {
				if _, err := tx.ExecContext(ctx, `INSERT INTO visit_tags (visit_id, tag_id) VALUES ($1,$2)
					ON CONFLICT DO NOTHING`, id, tid); err != nil {
					return 0, nil, fmt.Errorf("写入访问标签: %w", err)
				}
			}
		}
	}

	// 位移
	for _, m := range b.Movements {
		var dist any
		var fromPID, toPID any
		if m.FromVisitSrc != nil && m.ToVisitSrc != nil {
			fp, okf := visitPlace[*m.FromVisitSrc]
			tp, okt := visitPlace[*m.ToVisitSrc]
			if okf && okt {
				if id, ok := placeID[fp]; ok {
					fromPID = id
				}
				if id, ok := placeID[tp]; ok {
					toPID = id
				}
				c1, c2 := locCoord[fp], locCoord[tp]
				if c1[0] != 0 && c2[0] != 0 {
					d := geo.Haversine(c1[0], c1[1], c2[0], c2[1])
					dist = d
				}
			}
		}
		var tname, tcolor, ticon any
		if m.TransportSrc != nil {
			tname, tcolor, ticon = nullStr(transName[*m.TransportSrc]), nullStr(transColor[*m.TransportSrc]), nullStr(transIcon[*m.TransportSrc])
		}
		var dur any
		if m.Start != nil && m.End != nil {
			if d := int(m.End.Sub(*m.Start).Minutes()); d >= 0 {
				dur = d
			}
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO movements
			(dataset_id, user_id, src_pk, transport_src, transport_name, transport_color, transport_icon,
			 from_visit_src, to_visit_src, from_place_id, to_place_id, started_at, ended_at, duration_min, distance_km)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)
			ON CONFLICT DO NOTHING`,
			datasetID, userID, m.SrcPK, m.TransportSrc, tname, tcolor, ticon,
			m.FromVisitSrc, m.ToVisitSrc, fromPID, toPID, m.Start, m.End, dur, dist); err != nil {
			return 0, nil, fmt.Errorf("写入位移记录: %w", err)
		}
	}

	// 小时级天气（去重，仅保留整点）
	for _, w := range b.Weather {
		if w.At == nil {
			skip("ZHOURLYWEATHER", w.SrcPK)
			continue
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO weather
			(dataset_id, user_id, src_pk, visit_src, at, is_daylight, temperature_c, apparent_c, humidity,
			 precipitation_amount, precipitation_chance, wind_speed, visibility, uv_index,
			 condition, symbol, uv_category, wind_direction)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,NULLIF($15,''),NULLIF($16,''),NULLIF($17,''),NULLIF($18,''))
			ON CONFLICT DO NOTHING`,
			datasetID, userID, w.SrcPK, w.VisitSrc, w.At, w.IsDaylight, w.TempC, w.ApparentC, w.Humidity,
			w.PrecipAmt, w.PrecipPct, w.WindSpeed, w.Visibility, w.UVIndex,
			w.Condition, w.Symbol, w.UVCategory, w.WindDir); err != nil {
			return 0, nil, fmt.Errorf("写入天气记录: %w", err)
		}
	}

	// 回填地点聚合
	for src, a := range agg {
		if _, err := tx.ExecContext(ctx, `UPDATE places SET visit_count=$1, dwell_minutes=$2,
			first_visit_at=$3, last_visit_at=$4 WHERE id=$5`, a.count, a.dwell, a.first, a.last, placeID[src]); err != nil {
			return 0, nil, fmt.Errorf("回填地点统计: %w", err)
		}
	}

	// 汇总
	st := &Stats{Visits: vcount, Skipped: skipped, Places: len(b.Locations), RawVisits: b.RawVisitNum, FirstVisit: first, LastVisit: last}
	if _, err := tx.ExecContext(ctx, `UPDATE datasets SET status='done', visit_count=$1, place_count=$2,
		raw_visit_count=$3, first_visit_at=$4, last_visit_at=$5,
		source_created_at=$6, content_hash=$7 WHERE id=$8`,
		st.Visits, st.Places, st.RawVisits, first, last, time.Now(), hash, datasetID); err != nil {
		return 0, nil, fmt.Errorf("更新数据集汇总: %w", err)
	}

	// 快照式语义：最新导入的那一份接管展示，其余自动退居后台（后台仍可手动切回）
	if _, err := tx.ExecContext(ctx, `UPDATE datasets SET is_active = (id=$1) WHERE user_id=$2`,
		datasetID, userID); err != nil {
		return 0, nil, fmt.Errorf("切换生效数据集: %w", err)
	}

	// 原始行留档：站点只映射了一部分字段，把 Core Data 里的整行存下来，
	// 导出时逐列还原——这样既不受映射范围限制，也不依赖基线库去补剩下的列。
	if err := insertEntityRaw(ctx, tx, datasetID, b.RawRows, rawSkipped); err != nil {
		return 0, nil, err
	}

	// 坐标来源留档要等 entity_raw 写完才能做（原始 GPS 只在留档行里）。
	if _, err := BackfillCoordSource(ctx, tx, datasetID); err != nil {
		return 0, nil, err
	}

	if err := tx.Commit(); err != nil {
		return 0, nil, err
	}
	return datasetID, st, nil
}

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// Cached 记录已有上传的摘要，供重复上传时直接复用。
type Cached struct {
	DatasetID int64
	Stats     Stats
}

func SortLocations(list []rond.Location) {
	sort.Slice(list, func(i, j int) bool { return list[i].SrcPK < list[j].SrcPK })
}

func TempDir(base, name string) (string, error) {
	d := filepath.Join(base, fmt.Sprintf("import-%d", time.Now().UnixNano()))
	if err := os.MkdirAll(d, 0o700); err != nil {
		return "", err
	}
	return d, nil
}
