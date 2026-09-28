package server

import (
	"context"
	"time"

	"github.com/suxen-project/suxen/internal/domain"
	"github.com/suxen-project/suxen/internal/store"
)

const (
	cleanupLeaseName = "cleanup-scheduler"
	gcLeaseName      = "garbage-collection-scheduler"
	verifyLeaseName  = "verify-scheduler"
	migrateLeaseName = "migrate-scheduler"
)

// leaseCoordinator is the cluster-lease capability the singleton background
// workers and declarative provisioning use to elect a single holder: acquire
// or renew a named lease, and read a lease's current holder and expiry.
type leaseCoordinator interface {
	AcquireLease(context.Context, string, string, time.Time, time.Time) (bool, error)
	Lease(context.Context, string) (domain.Lease, error)
}

// leases narrows the metadata store to the cluster-lease capability. It reads
// s.metadata on each call so a reconfigured backend is honoured.
func (s *Server) leases() leaseCoordinator {
	return s.metadata
}

// taskJournal is the background-job task-record capability: the scheduled jobs
// (cleanup, gc, verify, migrate) record a task at start and update it on
// completion, and the task-listing handlers read one task or a page.
type taskJournal interface {
	CreateTask(context.Context, domain.Task) (domain.Task, error)
	UpdateTask(context.Context, domain.Task) error
	Task(context.Context, int64) (domain.Task, error)
	TaskPage(context.Context, int64, int64, int) (store.IDPage[domain.Task], error)
}

// tasks narrows the metadata store to the task-journal capability. It reads
// s.metadata on each call so a reconfigured backend is honoured.
func (s *Server) tasks() taskJournal {
	return s.metadata
}

// finishTask records the outcome even when the operation stopped because its
// request or scheduler context was canceled. Only this final bookkeeping write
// outlives that context, with a bounded timeout.
func (s *Server) finishTask(ctx context.Context, task domain.Task) error {
	completionCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	return s.tasks().UpdateTask(completionCtx, task)
}

const (
	schedulerLeaseDuration = 30 * time.Second
	schedulerLeaseRenewal  = 10 * time.Second
)

// StartScheduler starts background delivery, metrics, and scheduled job
// workers. Cluster mode gives each singleton worker its own renewable
// database lease. Calling StartScheduler more than once has no effect.
func (s *Server) StartScheduler(ctx context.Context) {
	s.schedulerOnce.Do(func() {
		go s.content.WebhookDeliveryWorker(ctx)
		go s.aggregateMetricWorker(ctx)
		if s.cfg.GCInterval > 0 {
			go s.localStagingReaper(ctx, s.cfg.GCInterval)
		}
		for _, job := range scheduledJobs {
			interval := job.interval(s.cfg)
			if interval <= 0 {
				continue
			}
			go s.jobScheduler(ctx, job, interval)
		}
	})
}

// Generic upload staging is local to each replica. Reap it on every replica;
// the cluster lease for physical blob GC must not suppress this local work.
func (s *Server) localStagingReaper(ctx context.Context, interval time.Duration) {
	reap := func() {
		if _, _, err := s.content.ReapStaleStaging(false, time.Now()); err != nil {
			s.log.Error("reap local upload staging", "error", err)
		}
	}
	reap()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			reap()
		}
	}
}

func (s *Server) jobScheduler(ctx context.Context, job scheduledJob, interval time.Duration) {
	s.runJobOnce(ctx, job)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.runJobOnce(ctx, job)
		}
	}
}

func (s *Server) runJobOnce(ctx context.Context, job scheduledJob) {
	s.runScheduledSingleton(
		ctx,
		job.leaseName,
		job.metricRole,
		job.name,
		func(ctx context.Context) {
			if err := job.run(s, ctx); err != nil {
				s.log.Error("run scheduled job", "job", job.name, "error", err)
			}
		},
	)
}

func (s *Server) runScheduledCleanup(ctx context.Context) {
	s.runNamedJobOnce(ctx, "cleanup")
}

func (s *Server) runScheduledGarbageCollection(ctx context.Context) {
	s.runNamedJobOnce(ctx, "gc")
}

func (s *Server) runNamedJobOnce(ctx context.Context, name string) {
	job, found := scheduledJobByName(name)
	if !found {
		s.log.Error("run named job", "job", name, "error", "unknown job")
		return
	}
	s.runJobOnce(ctx, job)
}

func (s *Server) runScheduledSingleton(
	ctx context.Context,
	leaseName string,
	metricRole string,
	workerName string,
	execute func(context.Context),
) {
	if !s.cfg.Cluster {
		execute(ctx)
		return
	}

	acquired, err := s.acquireSchedulerLease(
		ctx,
		leaseName,
		metricRole,
		schedulerLeaseDuration,
	)
	if err != nil {
		s.log.Error("acquire scheduler lease", "worker", workerName, "error", err)
		return
	}
	if !acquired {
		return
	}
	runContext, cancelRun := context.WithCancel(ctx)
	defer cancelRun()
	stopRenewal := s.renewSchedulerLease(
		runContext,
		cancelRun,
		leaseName,
		metricRole,
		workerName,
	)
	defer stopRenewal()
	execute(runContext)
}

func (s *Server) aggregateMetricWorker(ctx context.Context) {
	s.refreshAggregateMetrics(ctx)
	ticker := time.NewTicker(aggregateRefreshInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.refreshAggregateMetrics(ctx)
		}
	}
}

func (s *Server) refreshAggregateMetrics(ctx context.Context) {
	if err := s.metrics.refreshAggregates(ctx, s.metadata); err != nil {
		s.log.Warn("refresh aggregate metrics", "error", err)
	}
}

func (s *Server) renewSchedulerLease(
	ctx context.Context,
	cancelRun context.CancelFunc,
	leaseName string,
	metricRole string,
	workerName string,
) func() {
	stop := make(chan struct{})
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		ticker := time.NewTicker(schedulerLeaseRenewal)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-stop:
				return
			case <-ticker.C:
				acquired, err := s.acquireSchedulerLease(
					ctx,
					leaseName,
					metricRole,
					schedulerLeaseDuration,
				)
				if err != nil || !acquired {
					if err != nil {
						s.log.Error(
							"renew scheduler lease",
							"worker", workerName,
							"error", err,
						)
					}
					cancelRun()
					return
				}
			}
		}
	}()
	return func() {
		close(stop)
		<-stopped
	}
}

func (s *Server) acquireSchedulerLease(
	ctx context.Context,
	leaseName string,
	metricRole string,
	duration time.Duration,
) (bool, error) {
	now := time.Now().UTC()
	expiresAt := now.Add(duration)
	acquired, err := s.leases().AcquireLease(
		ctx,
		leaseName,
		s.schedulerID,
		now,
		expiresAt,
	)
	if err == nil && acquired {
		s.metrics.holdLeaderUntil(metricRole, expiresAt)
	} else if err == nil {
		s.metrics.releaseLeader(metricRole)
	}
	return acquired, err
}
