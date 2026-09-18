package fog

import (
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"time"
)

// Index 是某个用户迷雾位图的内存索引，按块粒度持有，
// 供瓦片渲染做范围查询；写路径先落库再刷新内存。
type Index struct {
	mu     sync.RWMutex
	blocks map[[2]int][]byte
	cells  int
	// areaM2 是全部置位格的实地面积（m²），写入索引时一并算好。
	// 覆盖率统计每次都要这个数，现算得把上万块的位图全扫一遍。
	areaM2 float64
	ver    int64 // 最近一次更新的 unix 秒，用作瓦片缓存版本号
}

// NewIndex 建一个空索引。
func NewIndex() *Index {
	return &Index{blocks: map[[2]int][]byte{}}
}

// Load 从库里加载某用户的全部迷雾块。
func (ix *Index) Load(db *sql.DB, userID int64) error {
	rows, err := db.Query(`SELECT gbx, gby, bitmap FROM fog_blocks WHERE user_id=$1`, userID)
	if err != nil {
		return err
	}
	defer rows.Close()
	blocks := map[[2]int][]byte{}
	cells, areaM2 := 0, 0.0
	for rows.Next() {
		var gx, gy int
		var bm []byte
		if err := rows.Scan(&gx, &gy, &bm); err != nil {
			return err
		}
		blocks[[2]int{gx, gy}] = bm
		n := popcountSum(bm)
		cells += n
		areaM2 += float64(n) * cellAreaM2(gx, gy)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	ix.mu.Lock()
	ix.blocks = blocks
	ix.cells = cells
	ix.areaM2 = areaM2
	ix.ver = time.Now().Unix()
	ix.mu.Unlock()
	return nil
}

// LoadAll 从库里加载全部迷雾块（站点为单用户展示，公开读路径不区分归属）。
func (ix *Index) LoadAll(db *sql.DB) error {
	rows, err := db.Query(`SELECT gbx, gby, bitmap FROM fog_blocks`)
	if err != nil {
		return err
	}
	defer rows.Close()
	blocks := map[[2]int][]byte{}
	cells, areaM2 := 0, 0.0
	for rows.Next() {
		var gx, gy int
		var bm []byte
		if err := rows.Scan(&gx, &gy, &bm); err != nil {
			return err
		}
		blocks[[2]int{gx, gy}] = bm
		n := popcountSum(bm)
		cells += n
		areaM2 += float64(n) * cellAreaM2(gx, gy)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	ix.mu.Lock()
	ix.blocks = blocks
	ix.cells = cells
	ix.areaM2 = areaM2
	ix.ver = time.Now().Unix()
	ix.mu.Unlock()
	return nil
}

// Stats 返回块数与格子数。
func (ix *Index) Stats() (blocks, cells int) {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	return len(ix.blocks), ix.cells
}

// AreaKm2 返回已探索面积（km²，按块中心纬度做 cos 修正）。写入索引时算好，这里只读。
func (ix *Index) AreaKm2() float64 {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	return ix.areaM2 / 1e6
}

// Version 返回数据版本号，随每次写入变化。
func (ix *Index) Version() int64 {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	return ix.ver
}

// View 在读锁内执行渲染查询，避免渲染中途被写换代。
func (ix *Index) View(fn func(blocks map[[2]int][]byte)) {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	fn(ix.blocks)
}

// Import 把解析出的块写入库：merge 模式按位或合并进已有数据，
// replace 模式先清空该用户全部迷雾再写入。完成后刷新内存索引。
func (ix *Index) Import(db *sql.DB, userID int64, blocks []Block, merge bool) (applied int, err error) {
	tx, err := db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	if !merge {
		if _, err := tx.Exec(`DELETE FROM fog_blocks WHERE user_id=$1`, userID); err != nil {
			return 0, err
		}
	}
	var upsert *sql.Stmt
	if upsert, err = tx.Prepare(`INSERT INTO fog_blocks (user_id, gbx, gby, bitmap, updated_at)
		VALUES ($1,$2,$3,$4,now())
		ON CONFLICT (user_id, gbx, gby) DO UPDATE SET bitmap=$4, updated_at=now()`); err != nil {
		return 0, err
	}
	defer upsert.Close()

	for _, b := range blocks {
		if merge {
			var old []byte
			err := tx.QueryRow(`SELECT bitmap FROM fog_blocks WHERE user_id=$1 AND gbx=$2 AND gby=$3`,
				userID, b.GX, b.GY).Scan(&old)
			switch {
			case err == nil && len(old) == len(b.Bitmap):
				for i := range b.Bitmap {
					b.Bitmap[i] |= old[i]
				}
			case err != nil && !errors.Is(err, sql.ErrNoRows):
				return 0, err
			}
		}
		if _, err := upsert.Exec(userID, b.GX, b.GY, b.Bitmap); err != nil {
			return 0, err
		}
		applied++
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	if err := ix.Load(db, userID); err != nil {
		return applied, err
	}
	return applied, nil
}

// CellLit 报告某个全局格是否已探索。
func (ix *Index) CellLit(cx, cy int) bool {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	bm, ok := ix.blocks[[2]int{cx / BlockCells, cy / BlockCells}]
	if !ok {
		return false
	}
	col, row := cx%BlockCells, cy%BlockCells
	return bm[row*8+col/8]&(0x80>>(col%8)) != 0
}

// All 返回全部块（导出用）。
func (ix *Index) All() []Block {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	out := make([]Block, 0, len(ix.blocks))
	for k, bm := range ix.blocks {
		out = append(out, Block{GX: k[0], GY: k[1], Bitmap: bm})
	}
	sortBlocks(out)
	return out
}

// LoadOrNewIndex 返回某用户的迷雾索引；首次访问时从库里加载。
func LoadOrNewIndex(db *sql.DB, userID int64, existing *Index) (*Index, error) {
	if existing != nil {
		return existing, nil
	}
	ix := NewIndex()
	if err := ix.Load(db, userID); err != nil {
		return nil, fmt.Errorf("加载迷雾数据: %w", err)
	}
	return ix, nil
}

// SaveRawLayers 保存快照里其它图层的原始条目（path -> zlib 流本身，一个字节都不动）。
// replace 为真时先清空（「整体替换」与「清空迷雾」都该把旧的一并带走），
// 否则同名覆盖——后传的快照更完整。
func (ix *Index) SaveRawLayers(db *sql.DB, userID int64, layers map[string][]byte, replace bool) error {
	if !replace && len(layers) == 0 {
		return nil
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if replace {
		if _, err := tx.Exec(`DELETE FROM fog_raw_files WHERE user_id=$1`, userID); err != nil {
			return err
		}
	}
	for p, data := range layers {
		if _, err := tx.Exec(`INSERT INTO fog_raw_files (user_id, path, data) VALUES ($1,$2,$3)
			ON CONFLICT (user_id, path) DO UPDATE SET data=EXCLUDED.data`, userID, p, data); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// DropRawLayers 丢弃留存的其它图层。清空迷雾时调用（与 fog_blocks 一样是站点级，
// 不按用户区分），否则导出的快照里会留着已经没有对应 Model/* 瓦片的图层条目。
func DropRawLayers(db *sql.DB) error {
	_, err := db.Exec(`DELETE FROM fog_raw_files`)
	return err
}

// RawLayers 读出留存的非已探索层文件（path -> 原始字节）。
func LoadRawLayers(db *sql.DB, userID int64) (map[string][]byte, error) {
	rows, err := db.Query(`SELECT path, data FROM fog_raw_files WHERE user_id=$1`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]byte{}
	for rows.Next() {
		var p string
		var data []byte
		if err := rows.Scan(&p, &data); err != nil {
			return nil, err
		}
		out[p] = data
	}
	return out, rows.Err()
}

// ---------- 迷雾遮罩区（对访客隐藏的范围） ----------

// SaveMask 新增或更新一块遮罩（ID 为 0 时新增），返回其 ID。
func SaveMask(db *sql.DB, userID int64, m Mask) (int64, error) {
	if m.ID > 0 {
		_, err := db.Exec(`UPDATE fog_masks SET name=$1, lat=$2, lon=$3, radius_m=$4 WHERE id=$5 AND user_id=$6`,
			m.Name, m.Lat, m.Lon, m.RadiusM, m.ID, userID)
		if err != nil {
			return 0, err
		}
		return m.ID, nil
	}
	var id int64
	err := db.QueryRow(`INSERT INTO fog_masks (user_id, name, lat, lon, radius_m) VALUES ($1,$2,$3,$4,$5)
		RETURNING id`, userID, m.Name, m.Lat, m.Lon, m.RadiusM).Scan(&id)
	return id, err
}

// DeleteMask 删除一块遮罩。
func DeleteMask(db *sql.DB, userID, id int64) error {
	_, err := db.Exec(`DELETE FROM fog_masks WHERE id=$1 AND user_id=$2`, id, userID)
	return err
}

// LoadMasks 读出全部遮罩。遮罩与迷雾一样是站点级数据（单站长的站点），
// 展示与渲染时不分归属，与 fog_blocks 的 LoadAll 保持一致。
func LoadMasks(db *sql.DB) ([]Mask, error) {
	rows, err := db.Query(`SELECT id, COALESCE(name,''), lat, lon, radius_m FROM fog_masks
		WHERE radius_m > 0 ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Mask
	for rows.Next() {
		var m Mask
		if err := rows.Scan(&m.ID, &m.Name, &m.Lat, &m.Lon, &m.RadiusM); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}
