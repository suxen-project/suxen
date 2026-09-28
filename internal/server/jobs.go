package server

import (
	"context"
	"time"

	"github.com/suxen-project/suxen/internal/config"
)

// scheduledJob is periodic singleton work owned by the host process.
//
// Jobs are a static list rather than a public registry on purpose: an
// out-of-tree job would need a host capability (metadata, blob stores, task
// history) to do anything useful, and publishing the identifier half without
// the execution half would freeze an interface that must still change. Each
// plugin's /api/v1/plugins/{id} namespace is separate from scheduled job
// paths such as POST /api/v1/gc.
type scheduledJob struct {
	name       string
	leaseName  string
	metricRole string
	interval   func(config.Config) time.Duration
	run        func(*Server, context.Context) error
}

var scheduledJobs = []scheduledJob{
	{
		name:       "gc",
		leaseName:  gcLeaseName,
		metricRole: gcSchedulerMetricRole,
		interval:   func(cfg config.Config) time.Duration { return cfg.GCInterval },
		run: func(s *Server, ctx context.Context) error {
			_, err := s.runGarbageCollection(ctx, false, defaultGCGracePeriod, "")
			return err
		},
	},
	{
		name:       "cleanup",
		leaseName:  cleanupLeaseName,
		metricRole: cleanupSchedulerMetricRole,
		interval:   func(cfg config.Config) time.Duration { return cfg.CleanupInterval },
		run:        (*Server).executeScheduledCleanup,
	},
	{
		name:       "verify",
		leaseName:  verifyLeaseName,
		metricRole: verifySchedulerMetricRole,
		interval:   func(cfg config.Config) time.Duration { return cfg.VerifyInterval },
		run: func(s *Server, ctx context.Context) error {
			// Scheduled runs cross-check every store without re-hashing; a
			// re-hash is an explicit, more expensive choice made per request.
			_, err := s.runBlobStoreVerify(ctx, "", false)
			return err
		},
	},
	{
		name:       "migrate",
		leaseName:  migrateLeaseName,
		metricRole: migrateSchedulerMetricRole,
		interval:   func(cfg config.Config) time.Duration { return cfg.MigrateInterval },
		run:        (*Server).runBlobStoreMigration,
	},
}

func scheduledJobByName(name string) (scheduledJob, bool) {
	for _, job := range scheduledJobs {
		if job.name == name {
			return job, true
		}
	}
	return scheduledJob{}, false
}
