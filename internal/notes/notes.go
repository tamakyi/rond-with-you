// Package notes 存放站长为地点加的别名与备注。
// 备注按 rond 的地点 ID（src_pk）而非数据库自增 id 存储，
// 因此重新上传备份、地点换了新 id 之后内容依然挂在原地。
package notes

import (
	"context"
	"database/sql"
	"strings"
	"time"
)

type Note struct {
	SrcPK     int
	Alias     string
	Content   string
	RondNote  string
	UpdatedAt time.Time
}

type Store struct{ DB *sql.DB }

// All 返回某个数据集的全站备注（站点只有一个站长账号，读的时候不按 user_id 过滤，
// 这样匿名访客也能看到别名；写的时候才需要登录用户的 id）。
// 后写覆盖先写，保证同一地点只保留一条。必须按 dataset_id 过滤：
// src_pk 只在单个数据集内唯一，跨数据集同编号是完全不同的地点。
func (s *Store) All(ctx context.Context, datasetID int64) (map[int]Note, error) {
	out := map[int]Note{}
	rows, err := s.DB.QueryContext(ctx,
		`SELECT src_pk, alias, note, rond_note, updated_at FROM place_notes
		  WHERE dataset_id=$1 ORDER BY updated_at DESC`, datasetID)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var n Note
		if err := rows.Scan(&n.SrcPK, &n.Alias, &n.Content, &n.RondNote, &n.UpdatedAt); err != nil {
			return out, err
		}
		out[n.SrcPK] = n
	}
	return out, rows.Err()
}

// SetNote 只改备注、保留别名（批量「清空备注」用）；两者都为空则删除记录。
func (s *Store) SetNote(ctx context.Context, userID, datasetID int64, srcPK int, content string) error {
	var alias string
	err := s.DB.QueryRowContext(ctx,
		`SELECT alias FROM place_notes WHERE user_id=$1 AND dataset_id=$2 AND src_pk=$3`,
		userID, datasetID, srcPK).Scan(&alias)
	if err != nil && err != sql.ErrNoRows {
		return err
	}
	return s.Save(ctx, userID, datasetID, srcPK, alias, content)
}

// Delete 删除某地点的整条备注与别名。
func (s *Store) Delete(ctx context.Context, userID, datasetID int64, srcPK int) error {
	_, err := s.DB.ExecContext(ctx,
		`DELETE FROM place_notes WHERE user_id=$1 AND dataset_id=$2 AND src_pk=$3`, userID, datasetID, srcPK)
	return err
}

// Save 写入别名与备注；两者都为空时清空手写内容（rond 回填的那一块要留着）。
func (s *Store) Save(ctx context.Context, userID, datasetID int64, srcPK int, alias, content string) error {
	alias = strings.TrimSpace(alias)
	content = strings.TrimSpace(content)
	if alias == "" && content == "" {
		// 没有 rond 备注时直接删行；有则只清空手写部分，别把回填结果一起删掉
		_, err := s.DB.ExecContext(ctx,
			`DELETE FROM place_notes WHERE user_id=$1 AND dataset_id=$2 AND src_pk=$3 AND rond_note=''`,
			userID, datasetID, srcPK)
		if err != nil {
			return err
		}
		_, err = s.DB.ExecContext(ctx,
			`UPDATE place_notes SET alias='', note='', updated_at=now()
			  WHERE user_id=$1 AND dataset_id=$2 AND src_pk=$3`, userID, datasetID, srcPK)
		return err
	}
	_, err := s.DB.ExecContext(ctx, `INSERT INTO place_notes (user_id, dataset_id, src_pk, alias, note, updated_at)
		VALUES ($1,$2,$3,$4,$5,now())
		ON CONFLICT (user_id, dataset_id, src_pk)
		DO UPDATE SET alias=EXCLUDED.alias, note=EXCLUDED.note, updated_at=now()`,
		userID, datasetID, srcPK, alias, content)
	return err
}

// BackfillRond 把 rond 备份里按次到访记录的备注（ZVISIT.ZREMARK_）汇总进 rond_note：
// 同一地点多条备注全部保留，去重后按时间升序、每条一行并带上日期。
// 只重写 rond_note 一列，站长手写的 note 一概不动。返回写了多少行。
func (s *Store) BackfillRond(ctx context.Context, userID, datasetID int64) (int, error) {
	// DISTINCT 在「日期 + 内容」这一层做，同一天重复写的同一句只留一条
	const agg = `
	WITH raw AS (
		SELECT p.src_pk, to_char(v.arrival, 'YYYY-MM-DD') || ' ' || btrim(v.remark) AS line
		FROM visits v JOIN places p ON p.id = v.place_id
		WHERE v.dataset_id = $1 AND btrim(COALESCE(v.remark, '')) <> ''
	), uniq AS (
		SELECT DISTINCT src_pk, line, left(line, 10) AS day FROM raw
	), agg AS (
		SELECT src_pk, string_agg(line, E'\n' ORDER BY day, line) AS txt FROM uniq GROUP BY src_pk
	)`
	res, err := s.DB.ExecContext(ctx, agg+`
		INSERT INTO place_notes (user_id, dataset_id, src_pk, alias, note, rond_note, updated_at)
		SELECT $2, $1, src_pk, '', '', txt, now() FROM agg
		ON CONFLICT (user_id, dataset_id, src_pk) DO UPDATE SET rond_note = EXCLUDED.rond_note`,
		datasetID, userID)
	if err != nil {
		return 0, err
	}
	affected, _ := res.RowsAffected()

	// 备注被删掉的地点要清掉旧值，否则会一直留着过期的 rond 内容。
	// 限定在本数据集的地点里：datasetID 不合法时子查询为空，一行都不会动，
	// 不会把别的数据集的回填结果误清掉。
	if _, err := s.DB.ExecContext(ctx, `
		UPDATE place_notes n SET rond_note = ''
		WHERE n.user_id = $2 AND n.dataset_id = $1 AND n.rond_note <> ''
		  AND n.src_pk IN (SELECT src_pk FROM places WHERE dataset_id = $1)
		  AND NOT EXISTS (
			SELECT 1 FROM visits v JOIN places p ON p.id = v.place_id
			WHERE v.dataset_id = $1 AND p.src_pk = n.src_pk AND btrim(COALESCE(v.remark, '')) <> ''
		  )`, datasetID, userID); err != nil {
		return 0, err
	}
	return int(affected), nil
}
