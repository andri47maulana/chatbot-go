// Package worker implements the bounded goroutine pool that processes
// incoming WhatsApp message jobs concurrently.
package worker

import (
	"context"
	"sync"
	"time"

	"github.com/aigri/whatsapp-bot/internal/model"
	"github.com/aigri/whatsapp-bot/internal/service"
	"go.uber.org/zap"
)

// Processor is a fixed-size worker pool.  Each worker pulls MessageJob
// values from the shared jobs channel and calls RoutingService.ProcessJob.
type Processor struct {
	jobs    <-chan model.MessageJob
	routing *service.RoutingService
	count   int
	log     *zap.Logger
}

// New creates a Processor.  count is the number of goroutines to spawn.
func New(
	jobs <-chan model.MessageJob,
	routing *service.RoutingService,
	count int,
	log *zap.Logger,
) *Processor {
	return &Processor{
		jobs:    jobs,
		routing: routing,
		count:   count,
		log:     log,
	}
}

// Start spawns count worker goroutines and blocks until ctx is cancelled.
// It also accepts an external WaitGroup so the caller can wait for drain.
func (p *Processor) Start(ctx context.Context, wg *sync.WaitGroup) {
	p.log.Info("worker pool starting", zap.Int("workers", p.count))

	for i := 0; i < p.count; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			p.work(ctx, id)
		}(i + 1)
	}
}

// work is the inner loop of a single worker goroutine.
func (p *Processor) work(ctx context.Context, id int) {
	log := p.log.With(zap.Int("worker", id))
	log.Debug("worker started")
	defer log.Debug("worker stopped")

	for {
		select {
		case <-ctx.Done():
			// Drain remaining in-flight jobs before exiting so we don't drop
			// messages that have already been popped off the channel.
			for {
				select {
				case job, ok := <-p.jobs:
					if !ok {
						return
					}
					p.process(ctx, job, log)
				default:
					return
				}
			}
		case job, ok := <-p.jobs:
			if !ok {
				return
			}
			p.process(ctx, job, log)
		}
	}
}

// process runs one job with a per-job timeout and panic recovery.
func (p *Processor) process(ctx context.Context, job model.MessageJob, log *zap.Logger) {
	defer func() {
		if rec := recover(); rec != nil {
			log.Error("panic processing job",
				zap.String("phone", job.PhoneNumber),
				zap.Any("recover", rec),
			)
		}
	}()

	// Give each job a generous but bounded deadline.
	timeout := 90 * time.Second
	jobCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	log.Debug("processing job",
		zap.String("phone", job.PhoneNumber),
		zap.String("chat_id", job.ChatID),
		zap.String("msg_id", job.MessageID),
	)

	p.routing.ProcessJob(jobCtx, job)
}
