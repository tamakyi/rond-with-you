// Package votes 存放地点的「推荐 / 踩雷」两类表态：
//   - place_votes         站长对地点的官方结论（单值）
//   - place_visitor_votes 访客点赞 / 点踩（一人一票，可改可撤）
//
// 两者都按 rond 的地点 ID（src_pk）存储，重新上传备份、地点换了新 id 也不丢。
package votes

import (
	"context"
	"database/sql"
)

// VerdictRecommend / VerdictAvoid 是官方结论的取值。
const (
	VerdictAvoid     = -1
	VerdictRecommend = 1
)

type Store struct{ DB *sql.DB }

// Tally 是一个地点的访客票数汇总。
type Tally struct {
	Up   int `json:"up"`
	Down int `json:"down"`
}

// Verdicts 返回某个数据集的站长结论：src_pk -> verdict(1/-1)。
// 必须按 dataset_id 过滤：src_pk 只在单个数据集内唯一。
func (s *Store) Verdicts(ctx context.Context, datasetID int64) (map[int]int, error) {
	out := map[int]int{}
	rows, err := s.DB.QueryContext(ctx, `SELECT src_pk, verdict FROM place_votes WHERE dataset_id=$1`, datasetID)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var pk, v int
		if err := rows.Scan(&pk, &v); err != nil {
			return out, err
		}
		out[pk] = v
	}
	return out, rows.Err()
}

// Verdict 取单个地点的站长结论；未标注返回 0。
func (s *Store) Verdict(ctx context.Context, datasetID int64, srcPK int) (int, error) {
	var v int
	err := s.DB.QueryRowContext(ctx,
		`SELECT verdict FROM place_votes WHERE dataset_id=$1 AND src_pk=$2`, datasetID, srcPK).Scan(&v)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	return v, err
}

// SaveVerdict 设置站长结论；verdict 为 0 表示取消结论。
func (s *Store) SaveVerdict(ctx context.Context, userID, datasetID int64, srcPK, verdict int) error {
	if verdict == 0 {
		_, err := s.DB.ExecContext(ctx,
			`DELETE FROM place_votes WHERE user_id=$1 AND dataset_id=$2 AND src_pk=$3`, userID, datasetID, srcPK)
		return err
	}
	_, err := s.DB.ExecContext(ctx, `INSERT INTO place_votes (user_id, dataset_id, src_pk, verdict, updated_at)
		VALUES ($1,$2,$3,$4,now())
		ON CONFLICT (user_id, dataset_id, src_pk) DO UPDATE SET verdict=EXCLUDED.verdict, updated_at=now()`,
		userID, datasetID, srcPK, verdict)
	return err
}

// Tallies 返回全部地点的访客票数。票表规模很小（个人站点），一次读全更省事，
// 也避免给 pgx 传数组参数带来的类型歧义。
func (s *Store) Tallies(ctx context.Context) (map[int]Tally, error) {
	out := map[int]Tally{}
	rows, err := s.DB.QueryContext(ctx, `SELECT src_pk,
		count(*) FILTER (WHERE vote=1), count(*) FILTER (WHERE vote=-1)
		FROM place_visitor_votes GROUP BY src_pk`)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var pk int
		var t Tally
		if err := rows.Scan(&pk, &t.Up, &t.Down); err != nil {
			return out, err
		}
		out[pk] = t
	}
	return out, rows.Err()
}

// Tally 取单个地点的访客票数。
func (s *Store) Tally(ctx context.Context, srcPK int) (Tally, error) {
	var t Tally
	err := s.DB.QueryRowContext(ctx, `SELECT count(*) FILTER (WHERE vote=1), count(*) FILTER (WHERE vote=-1)
		FROM place_visitor_votes WHERE src_pk=$1`, srcPK).Scan(&t.Up, &t.Down)
	return t, err
}

// VoterAll 返回该访客投过的全部票：src_pk -> vote，供地图一次性标注「我投的」。
func (s *Store) VoterAll(ctx context.Context, voterKey string) (map[int]int, error) {
	out := map[int]int{}
	if voterKey == "" {
		return out, nil
	}
	rows, err := s.DB.QueryContext(ctx,
		`SELECT src_pk, vote FROM place_visitor_votes WHERE voter_key=$1`, voterKey)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var pk, v int
		if err := rows.Scan(&pk, &v); err != nil {
			return out, err
		}
		out[pk] = v
	}
	return out, rows.Err()
}

// Voter 返回该访客对某地点的既有投票（1/-1/0）。voterKey 为空表示拿不到身份。
func (s *Store) Voter(ctx context.Context, srcPK int, voterKey string) int {
	if voterKey == "" {
		return 0
	}
	var v int
	if err := s.DB.QueryRowContext(ctx,
		`SELECT vote FROM place_visitor_votes WHERE src_pk=$1 AND voter_key=$2`, srcPK, voterKey).Scan(&v); err != nil {
		return 0
	}
	return v
}

// Cast 记录访客投票：未投过则新增；投同一票则撤销（再点一次取消）；投另一票则改为该票。
// 返回更新后的票数与该访客最终的选择。
func (s *Store) Cast(ctx context.Context, srcPK int, voterKey string, vote int) (Tally, int, error) {
	var cur int
	err := s.DB.QueryRowContext(ctx,
		`SELECT vote FROM place_visitor_votes WHERE src_pk=$1 AND voter_key=$2`, srcPK, voterKey).Scan(&cur)
	switch {
	case err == sql.ErrNoRows:
		_, err = s.DB.ExecContext(ctx,
			`INSERT INTO place_visitor_votes (src_pk, voter_key, vote) VALUES ($1,$2,$3)`, srcPK, voterKey, vote)
		cur = vote
	case err != nil:
		return Tally{}, 0, err
	case cur == vote:
		_, err = s.DB.ExecContext(ctx,
			`DELETE FROM place_visitor_votes WHERE src_pk=$1 AND voter_key=$2`, srcPK, voterKey)
		cur = 0
	default:
		_, err = s.DB.ExecContext(ctx,
			`UPDATE place_visitor_votes SET vote=$3, updated_at=now() WHERE src_pk=$1 AND voter_key=$2`,
			srcPK, voterKey, vote)
		cur = vote
	}
	if err != nil {
		return Tally{}, 0, err
	}
	t, err := s.Tally(ctx, srcPK)
	return t, cur, err
}
