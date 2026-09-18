package web

import (
	"context"
	"log"
	"net/http"
	"strconv"
	"strings"
)

// backfillNeed 是被判定为「信息不全」的地点：地址全空（区域黑名单匹配不上、统计里也没城市），
// 或者名字为空（地图上显示成「未命名」）。
type backfillNeed struct {
	ID                    int64
	Lat, Lon              float64
	Name                  string
	Province, City, Distr string
}

// backfillPlaceAddresses 给缺地址 / 没名字的地点补上反查结果。
//
// 只填空着的列，不覆盖已有值——宁可少改。名字为空时用最近的 POI 命名（和 rond 自己的做法一致），
// 周边 200m 没有收录就退回街道 / 乡镇；再取不到就只补地址，名字保持空。
// limit 用来兜住高德配额；返回值 filled 是补成功的条数，remain 是还剩下的候选数。
func (s *Server) backfillPlaceAddresses(ctx context.Context, datasetID int64, limit int) (filled, remain int, err error) {
	if s.cfg.GeocodeKey == "" {
		return 0, 0, nil // 没配 key，这个功能整体关闭
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id, lat, lon, COALESCE(name,''),
		COALESCE(province,''), COALESCE(city,''), COALESCE(district,'')
		FROM places
		WHERE dataset_id=$1 AND lat IS NOT NULL AND lon IS NOT NULL
		  AND (COALESCE(city,'')='' OR COALESCE(name,'')='')
		ORDER BY id`, datasetID)
	if err != nil {
		return 0, 0, err
	}
	var todo []backfillNeed
	for rows.Next() {
		var t backfillNeed
		if err := rows.Scan(&t.ID, &t.Lat, &t.Lon, &t.Name, &t.Province, &t.City, &t.Distr); err != nil {
			rows.Close()
			return 0, 0, err
		}
		todo = append(todo, t)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, 0, err
	}
	if len(todo) == 0 {
		return 0, 0, nil
	}
	if limit > 0 && len(todo) > limit {
		remain = len(todo) - limit
		todo = todo[:limit]
	}

	for _, t := range todo {
		if ctx.Err() != nil {
			break
		}
		_, prov, city, dist, street, town, ok := s.amapRegeo(ctx, t.Lat, t.Lon)
		if !ok {
			remain++
			continue
		}
		// 名字为空：优先用最近的 POI，退而用街道 / 乡镇
		name := t.Name
		if strings.TrimSpace(name) == "" {
			if poi, _, hit := s.amapNearestPOI(ctx, t.Lat, t.Lon); hit {
				name = poi
			} else if street != "" {
				name = street
			} else if town != "" {
				name = town
			}
		}
		// 只填空着的列
		newProv, newCity, newDist := t.Province, t.City, t.Distr
		if newProv == "" {
			newProv = prov
		}
		if newCity == "" {
			newCity = city
		}
		if newDist == "" {
			newDist = dist
		}
		if name == t.Name && newProv == t.Province && newCity == t.City && newDist == t.Distr {
			continue // 反查也没给出新信息
		}
		if _, err := s.db.ExecContext(ctx, `UPDATE places SET
			name = COALESCE(NULLIF($2,''), name),
			province = COALESCE(NULLIF($3,''), province),
			city = COALESCE(NULLIF($4,''), city),
			district = COALESCE(NULLIF($5,''), district)
			WHERE id=$1`, t.ID, name, newProv, newCity, newDist); err != nil {
			return filled, remain, err
		}
		filled++
	}
	return filled, remain, nil
}

// backfillAfterImport 是导入后的自动补全：只处理少量，别让上传等太久。
func (s *Server) backfillAfterImport(ctx context.Context, datasetID int64) string {
	filled, remain, err := s.backfillPlaceAddresses(ctx, datasetID, 20)
	if err != nil {
		log.Printf("导入后补全地址失败: %v", err)
		return ""
	}
	if filled == 0 && remain == 0 {
		return ""
	}
	msg := ""
	if filled > 0 {
		msg = "；已自动反查补全 " + strconv.Itoa(filled) + " 个缺地址的地点"
	}
	if remain > 0 {
		msg += "（还有 " + strconv.Itoa(remain) + " 个可在后台点「补全缺失地址」继续）"
	}
	return msg
}

// adminBackfillPlaces 手动把当前数据集里缺地址 / 没名字的地点补全一遍。
func (s *Server) adminBackfillPlaces(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	ds, err := s.datasetFor(ctx)
	if err != nil || ds == nil {
		s.adminFlash(w, r, "err", "还没有导入任何备份")
		return
	}
	if s.cfg.GeocodeKey == "" {
		s.adminFlash(w, r, "err", "没有配置 geocode_key（高德 Web 服务密钥），无法反查地址")
		return
	}
	filled, remain, err := s.backfillPlaceAddresses(ctx, ds.ID, 100)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	switch {
	case filled == 0 && remain == 0:
		s.adminFlash(w, r, "ok", "没有需要补全的地点：地址与名称都齐全")
	case remain > 0:
		s.adminFlash(w, r, "ok", "已补全 "+strconv.Itoa(filled)+" 个地点，还剩 "+strconv.Itoa(remain)+" 个，再点一次继续")
	default:
		s.adminFlash(w, r, "ok", "已补全 "+strconv.Itoa(filled)+" 个地点的地址 / 名称")
	}
}
