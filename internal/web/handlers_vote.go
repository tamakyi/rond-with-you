package web

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"net"
	"net/http"
	"strconv"
	"strings"

	"rond-with-you/internal/stats"
	"rond-with-you/internal/votes"
)

// voterCookie 给匿名访客分配稳定身份，用于「一人一票」去重。
const voterCookie = "rond_voter"

// clientIP 取访客 IP，优先用反代透传的 X-Forwarded-For 第一段。
func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if i := strings.IndexByte(xff, ','); i >= 0 {
			return strings.TrimSpace(xff[:i])
		}
		return strings.TrimSpace(xff)
	}
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}

// voterKeyFromRequest 从既有 cookie 推导匿名身份；没有 cookie 返回空串。
// GET 页面据此显示「你自己的票」，但不主动种 cookie。
func (s *Server) voterKeyFromRequest(r *http.Request) string {
	c, err := r.Cookie(voterCookie)
	if err != nil || len(c.Value) < 16 {
		return ""
	}
	return hashVoter(c.Value, clientIP(r))
}

// ensureVoterKey 在投票时调用：没有 cookie 就种一个，再返回匿名身份。
// 叠加 IP 后即便访客清 cookie 重来，也会被识别为同一来源，起轻度防刷作用。
func (s *Server) ensureVoterKey(w http.ResponseWriter, r *http.Request) string {
	c, err := r.Cookie(voterCookie)
	var uid string
	if err == nil && len(c.Value) >= 16 {
		uid = c.Value
	} else {
		b := make([]byte, 16)
		if _, err := rand.Read(b); err != nil {
			return ""
		}
		uid = hex.EncodeToString(b)
		http.SetCookie(w, &http.Cookie{
			Name: voterCookie, Value: uid, Path: "/",
			MaxAge: 3600 * 24 * 365, HttpOnly: true, SameSite: http.SameSiteLaxMode,
		})
	}
	return hashVoter(uid, clientIP(r))
}

func hashVoter(uid, ip string) string {
	sum := sha256.Sum256([]byte(uid + "|" + ip))
	return hex.EncodeToString(sum[:16])
}

// votePayload 是投票数据的对外结构（含访客票数、站长结论与「我投的」）。
// Admin / Public 让前端自己决定气泡里该显示「我的结论」还是「大家怎么说」。
type votePayload struct {
	Verdicts map[int]int         `json:"verdicts"`
	Tallies  map[int]votes.Tally `json:"tallies"`
	Mine     map[int]int         `json:"mine"`
	Admin    bool                `json:"admin"`
	Public   bool                `json:"public"`
}

// apiVotes 返回全部地点的结论、票数与当前访客自己的票，供地点库 / 地图一次性取回。
// 标记不对访客公开时，访客拿到空集。
func (s *Server) apiVotes(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	isAdmin := userFrom(ctx) != nil
	public := true
	if sets, err := s.sets.Load(ctx); err == nil {
		public = sets.MarksPublic
	}
	if !isAdmin && !public {
		s.json(w, votePayload{
			Verdicts: map[int]int{}, Tallies: map[int]votes.Tally{}, Mine: map[int]int{},
			Public: false,
		})
		return
	}
	var votesDatasetID int64
	if ds, err := s.datasetFor(ctx); err == nil && ds != nil {
		votesDatasetID = ds.ID
	}
	verdicts, err := s.votes.Verdicts(ctx, votesDatasetID)
	if err != nil {
		s.json(w, map[string]string{"error": err.Error()})
		return
	}
	tallies, err := s.votes.Tallies(ctx)
	if err != nil {
		s.json(w, map[string]string{"error": err.Error()})
		return
	}
	mine, err := s.votes.VoterAll(ctx, s.voterKeyFromRequest(r))
	if err != nil {
		s.json(w, map[string]string{"error": err.Error()})
		return
	}
	s.json(w, votePayload{Verdicts: verdicts, Tallies: tallies, Mine: mine, Admin: isAdmin, Public: public})
}

// apiVerdict 供地图气泡以 AJAX 设置站长结论（不刷新页面）。
func (s *Server) apiVerdict(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r.Context())
	if err := r.ParseForm(); err != nil {
		s.json(w, map[string]string{"error": "请求解析失败"})
		return
	}
	ds, err := s.datasetFor(r.Context())
	if err != nil || ds == nil {
		s.json(w, map[string]string{"error": "还没有可编辑的数据集"})
		return
	}
	srcPK, err := strconv.Atoi(r.PostFormValue("src_pk"))
	verdict, _ := strconv.Atoi(r.PostFormValue("verdict"))
	if err != nil {
		s.json(w, map[string]string{"error": "地点标识无效"})
		return
	}
	if verdict != votes.VerdictRecommend && verdict != votes.VerdictAvoid {
		verdict = 0
	}
	if err := s.votes.SaveVerdict(r.Context(), u.ID, ds.ID, srcPK, verdict); err != nil {
		s.json(w, map[string]string{"error": err.Error()})
		return
	}
	s.json(w, map[string]any{"verdict": verdict})
}

// apiVote 记录访客对某地点的点赞 / 点踩，返回最新票数与该访客的选择。
func (s *Server) apiVote(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.json(w, map[string]string{"error": "请求解析失败"})
		return
	}
	srcPK, err := strconv.Atoi(r.PostFormValue("src_pk"))
	vote, _ := strconv.Atoi(r.PostFormValue("vote"))
	if err != nil || (vote != 1 && vote != -1) {
		s.json(w, map[string]string{"error": "参数无效"})
		return
	}
	key := s.ensureVoterKey(w, r)
	if key == "" {
		s.json(w, map[string]string{"error": "无法识别访客"})
		return
	}
	t, mine, err := s.votes.Cast(r.Context(), srcPK, key, vote)
	if err != nil {
		s.json(w, map[string]string{"error": err.Error()})
		return
	}
	s.json(w, map[string]any{"up": t.Up, "down": t.Down, "mine": mine})
}

// voteTotals 汇总站长结论数量与访客票数，供统计页展示。
func (s *Server) voteTotals(ctx context.Context, datasetID int64) (rec, avoid, up, down int, err error) {
	err = s.db.QueryRowContext(ctx, `SELECT
		(SELECT count(*) FROM place_votes WHERE dataset_id=$1 AND verdict=1),
		(SELECT count(*) FROM place_votes WHERE dataset_id=$1 AND verdict=-1),
		(SELECT count(*) FROM place_visitor_votes WHERE vote=1),
		(SELECT count(*) FROM place_visitor_votes WHERE vote=-1)`, datasetID).
		Scan(&rec, &avoid, &up, &down)
	return
}

// verdictPlaces 返回站长标为某结论的地点，按到访次数排序。
func (s *Server) verdictPlaces(ctx context.Context, datasetID int64, verdict, limit int) ([]stats.Place, error) {
	q := `SELECT ` + stats.PlaceCols + ` FROM places p
		WHERE p.dataset_id=$1 AND EXISTS (SELECT 1 FROM place_votes pv
			WHERE pv.src_pk=p.src_pk AND pv.dataset_id=p.dataset_id AND pv.verdict=$2)
		ORDER BY p.visit_count DESC LIMIT ` + strconv.Itoa(limit)
	rows, err := s.db.QueryContext(ctx, q, datasetID, verdict)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []stats.Place
	for rows.Next() {
		var pl stats.Place
		if err := stats.ScanPlace(rows, &pl); err != nil {
			return nil, err
		}
		out = append(out, pl)
	}
	return out, rows.Err()
}

// adminPlaceVote 设置站长对地点的官方结论（1 推荐 / -1 踩雷 / 0 取消）。
func (s *Server) adminPlaceVote(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r.Context())
	if err := r.ParseForm(); err != nil {
		s.serverError(w, r, err)
		return
	}
	ds, err := s.datasetFor(r.Context())
	if err != nil || ds == nil {
		s.adminFlash(w, r, "err", "还没有可编辑的数据集")
		return
	}
	srcPK, err := strconv.Atoi(r.PostFormValue("src_pk"))
	verdict, _ := strconv.Atoi(r.PostFormValue("verdict"))
	if err != nil {
		s.adminFlash(w, r, "err", "地点标识无效")
		return
	}
	if verdict != votes.VerdictRecommend && verdict != votes.VerdictAvoid {
		verdict = 0
	}
	if err := s.votes.SaveVerdict(r.Context(), u.ID, ds.ID, srcPK, verdict); err != nil {
		s.serverError(w, r, err)
		return
	}
	next := r.PostFormValue("next")
	if !strings.HasPrefix(next, "/") {
		next = "/admin"
	}
	http.Redirect(w, r, next, http.StatusSeeOther)
}
