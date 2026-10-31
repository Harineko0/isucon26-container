package isureal

import (
	"sync"
	"sync/atomic"
)

// boundedMap は件数に上限のある sync.Map。上限を超えたら全部捨てる。
// 中身は DB で確かめ直せる手がかりだけなので、捨てても応答は変わらない (1 往復増えるだけ)。
type boundedMap struct {
	m     sync.Map
	n     atomic.Int64
	limit int64
}

func (b *boundedMap) Load(key any) (any, bool) { return b.m.Load(key) }

func (b *boundedMap) Store(key, value any) {
	if _, loaded := b.m.Swap(key, value); !loaded && b.n.Add(1) > b.limit {
		b.Clear()
	}
}

func (b *boundedMap) Delete(key any) {
	if _, loaded := b.m.LoadAndDelete(key); loaded {
		b.n.Add(-1)
	}
}

func (b *boundedMap) Clear() {
	b.m.Clear()
	b.n.Store(0)
}
