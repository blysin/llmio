package balancers

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"math"
	"strconv"
)

// SessionAffinity 会话亲和调度：按加权一致性哈希（Rendezvous / HRW）选路，
// 相同路由键在候选集不变时始终命中同一个关联，候选增减时只迁移原本落在该
// 关联上的键，因此不需要跨请求保存映射。
type SessionAffinity struct {
	store      map[uint]int
	routingKey string
	// avoid 记录本请求内被 429 的关联。确定性选路下仅降权常常仍会选中同一家
	// （权重小于 3 时甚至不降），重试会被白白耗尽；Pop 优先跳过这些关联，
	// 全部跳过时再回到全体，保证可用性。该集合不跨请求，也不影响 Empty 语义。
	avoid map[uint]struct{}
}

func NewSessionAffinity(items map[uint]int, routingKey string) *SessionAffinity {
	return &SessionAffinity{
		store:      items,
		routingKey: routingKey,
		avoid:      make(map[uint]struct{}),
	}
}

// unit 把 64 位哈希映射到 (0,1)：取高 53 位（float64 可精确表示的最大位宽）
// 并加 0.5 居中，最小值约 5.5e-17，避免 math.Log 得到 0 或 -Inf。
func unit(h uint64) float64 {
	return (float64(h>>11) + 0.5) / float64(uint64(1)<<53)
}

// score 是加权 Rendezvous 的标准打分：weight / -ln(u)，取最大者。
// 权重越大得分整体越高，命中概率与权重成正比；哈希不掺入权重，降权不会
// 打乱无关键的归属。
//
// 这里用 sha256 而不是 FNV：会话键往往是 "session-1"、"session-2" 这类结构化
// 短串，FNV 的雪崩性不足，权重悬殊时会整体偏向同一节点（单测可复现）。
func (s *SessionAffinity) score(id uint, weight int) float64 {
	key := s.routingKey + "|" + strconv.FormatUint(uint64(id), 10)
	sum := sha256.Sum256([]byte(key))
	h := binary.BigEndian.Uint64(sum[:8])
	return -float64(weight) / math.Log(unit(h))
}

func (s *SessionAffinity) pick(skip map[uint]struct{}) (uint, bool) {
	var best uint
	bestScore := math.Inf(-1)
	found := false
	for id, weight := range s.store {
		if weight <= 0 {
			continue
		}
		if _, skipped := skip[id]; skipped {
			continue
		}
		score := s.score(id, weight)
		// map 迭代顺序随机，同分时必须用 id 兜底，否则同一键会抖动。
		if !found || score > bestScore || (score == bestScore && id < best) {
			best, bestScore, found = id, score, true
		}
	}
	return best, found
}

func (s *SessionAffinity) Pop() (uint, error) {
	if len(s.store) == 0 {
		return 0, fmt.Errorf("no provide items or all items are disabled")
	}
	if id, ok := s.pick(s.avoid); ok {
		return id, nil
	}
	// 所有候选都在本请求内被 429 过，仍给一次机会。
	if id, ok := s.pick(nil); ok {
		return id, nil
	}
	return 0, fmt.Errorf("total provide weight must be greater than 0")
}

func (s *SessionAffinity) Delete(key uint) {
	delete(s.store, key)
	delete(s.avoid, key)
}

func (s *SessionAffinity) Reduce(key uint) {
	if weight, ok := s.store[key]; ok {
		s.store[key] = weight - weight/3
	}
	s.avoid[key] = struct{}{}
}

func (s *SessionAffinity) Success(uint) {}

func (s *SessionAffinity) Empty() bool {
	return len(s.store) == 0
}
