package job

import (
	"context"
	"os"

	"github.com/berachain/offchain-sdk/log"
	"github.com/berachain/offchain-sdk/worker"
	workertypes "github.com/berachain/offchain-sdk/worker/types"
)

type Manager struct {
	// logger is the logger for the baseapp
	logger log.Logger

	// list of jobs
	jobs []Basic

	// Job producers are a pool of workers that produce jobs. These workers
	// run in the background and produce jobs that are then consumed by the
	// job executors.
	jobProducers worker.Pool

	// Job executors are a pool of workers that execute jobs. These workers
	// are fed jobs by the job producers.
	jobExecutors worker.Pool
}

// New creates a new baseapp.
func NewManager(
	name string,
	logger log.Logger,
	jobs []Basic,
) *Manager {
	// TODO: read from config.
	poolCfg := worker.DefaultPoolConfig()
	poolCfg.Name = name
	poolCfg.PrometheusPrefix = "job_executor"
	return &Manager{
		logger:       log.NewBlankLogger(os.Stdout),
		jobs:         jobs,
		jobExecutors: *worker.NewPool(poolCfg, logger),
		jobProducers: *worker.NewPool(&worker.PoolConfig{
			Name:             "job-producer",
			PrometheusPrefix: "job_producer",
			MinWorkers:       len(jobs),
			MaxWorkers:       len(jobs) + 1, // TODO: figure out why we need to +1
			ResizingStrategy: "eager",
			MaxQueuedJobs:    len(jobs),
		}, logger),
	}
}

// Start.
//
//nolint:gocognit // todo: fix.
func (jm *Manager) Start(ctx context.Context) {
	for _, j := range jm.jobs {
		if sj, ok := j.(HasSetup); ok {
			if err := sj.Setup(ctx); err != nil {
				panic(err)
			}
		}

		if condJob, ok := j.(Conditional); ok { //nolint:nestif // todo:fix.
			wrappedJob := WrapConditional(condJob)
			jm.jobExecutors.Submit(
				func() {
					if err := wrappedJob.Producer(ctx, &jm.jobExecutors); err != nil {
						jm.logger.Error("error in job producer", "err", err)
					}
				},
			)
		} else if pollJob, ok := j.(Polling); ok { //nolint:govet // todo fix.
			wrappedJob := WrapPolling(pollJob)
			jm.jobExecutors.Submit(
				func() {
					if err := wrappedJob.Producer(ctx, &jm.jobExecutors); err != nil {
						jm.logger.Error("error in job producer", "err", err)
					}
				},
			)
		} else if subJob, ok := j.(Subscribable); ok { //nolint:govet // todo fix.
			jm.jobExecutors.Submit(func() {
				ch := subJob.Subscribe(ctx)
				for {
					select {
					case val := <-ch:
						_ = jm.jobExecutors.SubmitJob(workertypes.NewPayload(ctx, subJob, val))
					case <-ctx.Done():
						return
					default:
						continue
					}
				}
			})
		} else if ethSubJob, ok := j.(EthSubscribable); ok { //nolint:govet // todo fix.
			jm.jobExecutors.Submit(func() {
				sub, ch := ethSubJob.Subscribe(ctx)
				for {
					select {
					case <-ctx.Done():
						ethSubJob.Unsubscribe(ctx)
						return
					case err := <-sub.Err():
						jm.logger.Error("error in subscription", "err", err)
						// TODO: add retry mechanism
						ethSubJob.Unsubscribe(ctx)
						return
					case val := <-ch:
						_ = jm.jobExecutors.SubmitJob(workertypes.NewPayload(ctx, ethSubJob, val))
						continue
					}
				}
			})
		} else {
			panic("unknown job type")
		}
	}
}

// Stop.
func (jm *Manager) Stop() {
	for _, j := range jm.jobs {
		if tj, ok := j.(HasTeardown); ok {
			if err := tj.Teardown(); err != nil {
				panic(err)
			}
		}
	}
}