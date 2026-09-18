package ingest

import (
	"context"
	"database/sql"
	"fmt"

	"rond-with-you/internal/geo"
)

// querier 让回填既能跑在导入事务里，也能跑在启动时的连接池上。
type querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// BackfillCoordSource 给「有 ZLOCATION 留档」的地点补坐标来源，并自愈两类历史错误。
//
// 判据不能是「有留档行」——站点导出的备份会给新建地点合成 ZLOCATION 行，把
// ZRAWLATITUDE 写成 GCJ02ToWGS84(导出那一刻的显示坐标)。这份包被回导之后，合成行
// 会被误当成真机的原始 GPS，于是照片导入的点被标成「rond 导入」。区分办法就是这条
// 往返关系本身：
//
//	ZRAWLATITUDE == GCJ02ToWGS84(ZLATITUDE)   （位级相等，因为值就是我们自己算的）
//
// 真机行的 ZRAW 是独立采样值，从不满足（实测基线 321 行零命中）。判据必须拿留档行
// **自己的** ZLATITUDE 去比，而不是 places.lat——后者会被后来的坐标编辑改掉。
//
// 顺带修一处历史笔误：早期回填把 src_lon 写成了显示经度 ZLONGITUDE，应为原始经度
// ZRAWLONGITUDE。按「src_lon 恰好等于显示经度」精确识别后改写，不碰用户手工订正过的值。
func BackfillCoordSource(ctx context.Context, q querier, datasetID int64) (int, error) {
	rows, err := q.QueryContext(ctx, `SELECT p.src_pk, COALESCE(p.coord_source,''), p.src_lat, p.src_lon,
		(r.raw->>'ZLATITUDE')::float8, (r.raw->>'ZLONGITUDE')::float8,
		(r.raw->>'ZRAWLATITUDE')::float8, (r.raw->>'ZRAWLONGITUDE')::float8
	  FROM places p JOIN entity_raw r
	    ON r.dataset_id=p.dataset_id AND r.entity='ZLOCATION' AND r.src_pk=p.src_pk
	 WHERE p.dataset_id=$1
	   AND (r.raw->>'ZLATITUDE')     ~ '^-?[0-9.]+$'
	   AND (r.raw->>'ZLONGITUDE')    ~ '^-?[0-9.]+$'
	   AND (r.raw->>'ZRAWLATITUDE')  ~ '^-?[0-9.]+$'
	   AND (r.raw->>'ZRAWLONGITUDE') ~ '^-?[0-9.]+$'`, datasetID)
	if err != nil {
		return 0, fmt.Errorf("读留档行: %w", err)
	}
	defer rows.Close()

	var fillPK []int64
	var fillLat, fillLon []float64
	var clearPK, fixPK []int64
	var fixLat, fixLon []float64

	for rows.Next() {
		var pk int64
		var source string
		var srcLat, srcLon sql.NullFloat64
		var zlat, zlon, rawLat, rawLon float64
		if err := rows.Scan(&pk, &source, &srcLat, &srcLon, &zlat, &zlon, &rawLat, &rawLon); err != nil {
			return 0, err
		}
		gLat, gLon := geo.GCJ02ToWGS84(zlat, zlon)
		if gLat == zlat && gLon == zlon {
			// 境外点不做 GCJ 偏移，ZRAW 与显示坐标本来就该相等。可合成行写的是
			// GCJ02ToWGS84(显示坐标)，在境外同样是恒等——两边长得一模一样，判不出来，
			// 干脆不碰：宁可留空，也别把真机的境外点清掉。
			continue
		}
		if gLat == rawLat && gLon == rawLon {
			// 合成行：早期回填按「有留档」误判成了 rond，撤回它写下的那份
			if source == "rond" && srcLat.Valid && srcLat.Float64 == rawLat {
				clearPK = append(clearPK, pk)
			}
			continue
		}
		switch {
		case source == "":
			fillPK = append(fillPK, pk)
			fillLat = append(fillLat, rawLat)
			fillLon = append(fillLon, rawLon)
		case source == "rond" && srcLon.Valid && srcLon.Float64 == zlon && srcLon.Float64 != rawLon:
			fixPK = append(fixPK, pk)
			fixLat = append(fixLat, rawLat)
			fixLon = append(fixLon, rawLon)
		}
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}

	changed := 0
	if len(fillPK) > 0 {
		res, err := q.ExecContext(ctx, `UPDATE places p SET coord_source='rond', coord_sys='wgs84',
			src_lat=v.rlat, src_lon=v.rlon
		  FROM (SELECT unnest($2::bigint[]) AS pk, unnest($3::float8[]) AS rlat,
		               unnest($4::float8[]) AS rlon) v
		 WHERE p.dataset_id=$1 AND p.src_pk=v.pk AND COALESCE(p.coord_source,'')=''`,
			datasetID, fillPK, fillLat, fillLon)
		if err != nil {
			return changed, fmt.Errorf("补坐标来源: %w", err)
		}
		if n, _ := res.RowsAffected(); n > 0 {
			changed += int(n)
		}
	}
	if len(fixPK) > 0 {
		res, err := q.ExecContext(ctx, `UPDATE places p SET src_lat=v.rlat, src_lon=v.rlon
		  FROM (SELECT unnest($2::bigint[]) AS pk, unnest($3::float8[]) AS rlat,
		               unnest($4::float8[]) AS rlon) v
		 WHERE p.dataset_id=$1 AND p.src_pk=v.pk`,
			datasetID, fixPK, fixLat, fixLon)
		if err != nil {
			return changed, fmt.Errorf("修正原始坐标: %w", err)
		}
		if n, _ := res.RowsAffected(); n > 0 {
			changed += int(n)
		}
	}
	if len(clearPK) > 0 {
		res, err := q.ExecContext(ctx, `UPDATE places SET coord_source=NULL, coord_sys=NULL,
			src_lat=NULL, src_lon=NULL
		 WHERE dataset_id=$1 AND src_pk=ANY($2::bigint[]) AND coord_source='rond'`,
			datasetID, clearPK)
		if err != nil {
			return changed, fmt.Errorf("撤回误判的来源: %w", err)
		}
		if n, _ := res.RowsAffected(); n > 0 {
			changed += int(n)
		}
	}
	return changed, nil
}

// BackfillAllCoordSource 对所有数据集跑一遍回填，供启动时自愈历史数据用。
// 单个数据集失败不阻断启动——留档是增强，不该挡住服务。
func BackfillAllCoordSource(ctx context.Context, d *sql.DB) error {
	rows, err := d.QueryContext(ctx, `SELECT id FROM datasets ORDER BY id`)
	if err != nil {
		return err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, id := range ids {
		if _, err := BackfillCoordSource(ctx, d, id); err != nil {
			return fmt.Errorf("数据集 %d: %w", id, err)
		}
	}
	return nil
}
