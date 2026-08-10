package schedular

import (
	"github.com/go-co-op/gocron/v2"
	"log/slog"
	"magnet-feed-sync/app/config"
)

type Service struct {
	scheduler gocron.Scheduler
	cfg       *config.Config
}

func NewService(cfg *config.Config) (*Service, error) {
	s, err := gocron.NewScheduler()
	if err != nil {
		return nil, err
	}

	return &Service{
		scheduler: s,
		cfg:       cfg,
	}, nil
}

func (s *Service) Start(cb func()) error {
	j, err := s.scheduler.NewJob(
		gocron.CronJob(s.cfg.Cron, false),
		gocron.NewTask(cb),
		// a sweep can outlive its interval (a cold flaresolverr solve is ~74s per task);
		// overlapping runs would double-probe the breaker and race on the run state
		gocron.WithSingletonMode(gocron.LimitModeReschedule),
	)
	if err != nil {
		return err
	}

	slog.Info("job created", "job_id", j.ID())

	s.scheduler.Start()

	return nil
}
