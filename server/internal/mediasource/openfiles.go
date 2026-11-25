package mediasource

import (
	"sort"
	"sync"
)

type openFiles struct {
	mu      sync.Mutex
	byIndex map[int]int
}

func (o *openFiles) acquire(index int) (release func()) {
	o.mu.Lock()
	if o.byIndex == nil {
		o.byIndex = make(map[int]int)
	}
	o.byIndex[index]++
	o.mu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			o.mu.Lock()
			defer o.mu.Unlock()
			o.byIndex[index]--
			if o.byIndex[index] <= 0 {
				delete(o.byIndex, index)
			}
		})
	}
}

func (o *openFiles) indices() []int {
	o.mu.Lock()
	defer o.mu.Unlock()
	out := make([]int, 0, len(o.byIndex))
	for i := range o.byIndex {
		out = append(out, i)
	}
	sort.Ints(out)
	return out
}
