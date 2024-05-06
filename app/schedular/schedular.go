package schedular

import (
	"github.com/go-co-op/gocron/v2"
	"log"
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
	)
	if err != nil {
		return err
	}

	log.Printf("[INFO] Job created: %s", j.ID())

	s.scheduler.Start()

	return nil
}
