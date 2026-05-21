package pool

import (
	"context"
	"fmt"
	"sync"
)

type ProcessPool struct {
	sem     chan struct{}
	maxSize int
	mu      sync.Mutex
	active  int
}

func New(maxConcurrent int) *ProcessPool {
	return &ProcessPool{
		sem:     make(chan struct{}, maxConcurrent),
		maxSize: maxConcurrent,
	}
}

func (p *ProcessPool) Acquire(ctx context.Context) error {
	select {
	case p.sem <- struct{}{}:
		p.mu.Lock()
		p.active++
		p.mu.Unlock()
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (p *ProcessPool) TryAcquire() bool {
	select {
	case p.sem <- struct{}{}:
		p.mu.Lock()
		p.active++
		p.mu.Unlock()
		return true
	default:
		return false
	}
}

func (p *ProcessPool) Release() {
	<-p.sem
	p.mu.Lock()
	p.active--
	p.mu.Unlock()
}

func (p *ProcessPool) Active() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.active
}

func (p *ProcessPool) Available() int {
	return p.maxSize - p.Active()
}

func (p *ProcessPool) QueuePosition() int {
	return len(p.sem)
}

func (p *ProcessPool) Status() string {
	return fmt.Sprintf("active=%d available=%d max=%d queue=%d",
		p.Active(), p.Available(), p.maxSize, p.QueuePosition())
}
