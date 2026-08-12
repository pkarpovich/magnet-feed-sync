package schedular

import (
	"fmt"
	"log/slog"

	"github.com/go-co-op/gocron/v2"
)

type Service struct {
	scheduler gocron.Scheduler
}

func NewService() (*Service, error) {
	s, err := gocron.NewScheduler()
	if err != nil {
		return nil, err
	}

	return &Service{scheduler: s}, nil
}

// singleton mode: both sweeps can outlive their interval, and an overlapping watcher run
// would publish twice and race the shared ext.to cookie
func (s *Service) AddJob(name, cronExpr string, cb func()) error {
	j, err := s.scheduler.NewJob(
		gocron.CronJob(cronExpr, false),
		gocron.NewTask(cb),
		gocron.WithName(name),
		gocron.WithSingletonMode(gocron.LimitModeReschedule),
	)
	if err != nil {
		return fmt.Errorf("create job %s: %w", name, err)
	}

	slog.Info("job created", "job", name, "job_id", j.ID())

	return nil
}

func (s *Service) Start() {
	s.scheduler.Start()
}
