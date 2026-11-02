package balancers

import (
	"fmt"
	"math"
	"testing"
)

func TestSessionAffinitySticky(t *testing.T) {
	items := map[uint]int{1: 1, 2: 1, 3: 2}

	first, err := NewSessionAffinity(items, "session-a").Pop()
	if err != nil {
		t.Fatalf("Pop() unexpected error: %v", err)
	}
	// 每次重建都走一遍随机 map 迭代序，结果必须仍然一致。
	for i := 0; i < 100; i++ {
		got, err := NewSessionAffinity(items, "session-a").Pop()
		if err != nil {
			t.Fatalf("Pop() unexpected error: %v", err)
		}
		if got != first {
			t.Fatalf("Pop() = %d, want %d across rebuilds", got, first)
		}
	}

	balancer := NewSessionAffinity(items, "session-a")
	for i := 0; i < 10; i++ {
		got, err := balancer.Pop()
		if err != nil {
			t.Fatalf("Pop() unexpected error: %v", err)
		}
		if got != first {
			t.Fatalf("repeated Pop() = %d, want %d", got, first)
		}
	}
}

func TestSessionAffinityDistributionFollowsWeights(t *testing.T) {
	tests := []struct {
		name  string
		items map[uint]int
		node  uint
		ratio float64
		tol   float64
		total int
	}{
		{name: "等权", items: map[uint]int{1: 1, 2: 1}, node: 2, ratio: 0.5, tol: 0.05, total: 2000},
		{name: "1:3", items: map[uint]int{1: 1, 2: 3}, node: 2, ratio: 0.75, tol: 0.05, total: 2000},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hits := 0
			for i := 0; i < tt.total; i++ {
				got, err := NewSessionAffinity(tt.items, fmt.Sprintf("session-%d", i)).Pop()
				if err != nil {
					t.Fatalf("Pop() unexpected error: %v", err)
				}
				if got == tt.node {
					hits++
				}
			}
			ratio := float64(hits) / float64(tt.total)
			if math.Abs(ratio-tt.ratio) > tt.tol {
				t.Fatalf("ratio = %.3f, want %.3f ± %.2f", ratio, tt.ratio, tt.tol)
			}
		})
	}
}

func TestSessionAffinityDelete(t *testing.T) {
	items := map[uint]int{1: 1, 2: 1, 3: 1}

	first, err := NewSessionAffinity(items, "session-delete").Pop()
	if err != nil {
		t.Fatalf("Pop() unexpected error: %v", err)
	}

	a := NewSessionAffinity(items, "session-delete")
	a.Delete(first)
	gotA, err := a.Pop()
	if err != nil {
		t.Fatalf("Pop() after Delete unexpected error: %v", err)
	}
	if gotA == first {
		t.Fatalf("Pop() = %d after Delete(%d), want another item", gotA, first)
	}
	if a.Empty() {
		t.Fatal("Empty() = true with two items left, want false")
	}

	// 同一删除动作在不同实例上必须得到相同的替代者。
	b := NewSessionAffinity(items, "session-delete")
	b.Delete(first)
	gotB, err := b.Pop()
	if err != nil {
		t.Fatalf("Pop() unexpected error: %v", err)
	}
	if gotB != gotA {
		t.Fatalf("next item = %d and %d, want deterministic", gotA, gotB)
	}

	a.Delete(1)
	a.Delete(2)
	a.Delete(3)
	if !a.Empty() {
		t.Fatal("Empty() = false after deleting all items, want true")
	}
	if _, err := a.Pop(); err == nil {
		t.Fatal("Pop() on empty balancer = nil error, want error")
	}
}

func TestSessionAffinityReduceAvoidsNode(t *testing.T) {
	balancer := NewSessionAffinity(map[uint]int{1: 1, 2: 1}, "session-429")

	first, err := balancer.Pop()
	if err != nil {
		t.Fatalf("Pop() unexpected error: %v", err)
	}
	balancer.Reduce(first)

	second, err := balancer.Pop()
	if err != nil {
		t.Fatalf("Pop() after Reduce unexpected error: %v", err)
	}
	if second == first {
		t.Fatalf("Pop() = %d after Reduce(%d), want the other item", second, first)
	}
	if balancer.Empty() {
		t.Fatal("Empty() = true after Reduce, want false")
	}

	// 全部被 429 后不能报错，仍要给出候选。
	balancer.Reduce(second)
	third, err := balancer.Pop()
	if err != nil {
		t.Fatalf("Pop() with all items avoided unexpected error: %v", err)
	}
	if third != first && third != second {
		t.Fatalf("Pop() = %d, want one of %d/%d", third, first, second)
	}
}

func TestSessionAffinityEmptyAndZeroWeight(t *testing.T) {
	empty := NewSessionAffinity(map[uint]int{}, "k")
	if !empty.Empty() {
		t.Fatal("Empty() = false on empty pool, want true")
	}
	if _, err := empty.Pop(); err == nil {
		t.Fatal("Pop() on empty pool = nil error, want error")
	}

	zero := NewSessionAffinity(map[uint]int{1: 0, 2: -1}, "k")
	if zero.Empty() {
		t.Fatal("Empty() = true with zero-weight items, want false")
	}
	if _, err := zero.Pop(); err == nil {
		t.Fatal("Pop() with no positive weight = nil error, want error")
	}
}
