package schedular

import (
	"github.com/go-co-op/gocron/v2"
	"log"
)

type Service struct {
	scheduler gocron.Scheduler
}

func NewService() (*Service, error) {
	s, err := gocron.NewScheduler()
	if err != nil {
		return nil, err
	}

	return &Service{
		scheduler: s,
	}, nil
}

func (s *Service) Start(cb func()) error {
	j, err := s.scheduler.NewJob(
		gocron.CronJob("*/1 * * * *", false),
		gocron.NewTask(cb),
	)
	if err != nil {
		return err
	}

	log.Printf("[INFO] Job created: %s", j.ID())

	s.scheduler.Start()

	return nil
}
