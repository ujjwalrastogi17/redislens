package report

import (
	"sync"

	"github.com/ujjwalrastogi17/redislens/internal/pattern"
)

// The RDB format can only be parsed in order, but classifying keys is
// independent per key. So one goroutine (the caller of Walk) parses and
// packs keys into batches, and worker goroutines classify them into their own
// tries, which are merged at the end. No locks: each trie has one owner.
//
//	Walk ──add──▶ batch ──work──▶ worker 1..N (Normalize + Insert into own trie)
//	  ▲                               │
//	  └──────────── free ◀────────────┘   (batches are reused)

// batchSize trades channel overhead (smaller) against memory and idle workers
// at the end of a run (larger). 4096 keys is ~250KB of keys per batch.
const batchSize = 4096

type item struct {
	end  int    // end offset of this key in batch.keys
	kind string // constant from rdb.Kind; doesn't allocate
	key  pattern.Key
}

// batch stores keys back to back in one buffer, so filling it doesn't allocate
// once its buffers have grown.
type batch struct {
	keys  []byte
	items []item
}

type pipeline struct {
	cur   *batch
	work  chan *batch
	free  chan *batch
	tries []*pattern.Trie
	wg    sync.WaitGroup
}

func newPipeline(workers, maxChildren int) *pipeline {
	p := &pipeline{
		work:  make(chan *batch, workers),
		free:  make(chan *batch, 2*workers+1),
		tries: make([]*pattern.Trie, workers),
	}
	// Every batch is either being filled, queued, being processed or free,
	// so 2*workers+1 batches always suffice and add never waits on allocation.
	for range 2*workers + 1 {
		p.free <- &batch{}
	}
	p.cur = <-p.free
	for i := range workers {
		p.tries[i] = pattern.NewTrie(maxChildren)
		p.wg.Add(1)
		go p.worker(p.tries[i])
	}
	return p
}

func (p *pipeline) worker(trie *pattern.Trie) {
	defer p.wg.Done()
	var tmpl []byte
	for b := range p.work {
		start := 0
		for _, it := range b.items {
			tmpl = pattern.AppendNormalized(tmpl[:0], b.keys[start:it.end])
			trie.Insert(tmpl, it.kind, it.key)
			start = it.end
		}
		b.keys, b.items = b.keys[:0], b.items[:0]
		p.free <- b
	}
}

// add copies key into the current batch, which is sent once full.
func (p *pipeline) add(raw []byte, kind string, k pattern.Key) {
	b := p.cur
	b.keys = append(b.keys, raw...)
	b.items = append(b.items, item{len(b.keys), kind, k})
	if len(b.items) == batchSize {
		p.work <- b
		p.cur = <-p.free
	}
}

// finish sends the last partial batch, waits for the workers and merges their tries.
func (p *pipeline) finish() *pattern.Trie {
	if len(p.cur.items) > 0 {
		p.work <- p.cur
	}
	close(p.work)
	p.wg.Wait()
	trie := p.tries[0]
	for _, t := range p.tries[1:] {
		trie.Merge(t)
	}
	return trie
}
