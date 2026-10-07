// Package sql emulates the sql service. (stub — to be implemented)
package sql

import (
	"context"

	"github.com/linuxuser586/gcpemu/internal/emu"
)

// Service is the sql service module.
type Service struct{ env *emu.Env }

// New returns the service.
func New(env *emu.Env) emu.Service { return &Service{env: env} }

func (s *Service) Name() string                    { return "sql" }
func (s *Service) Register(r emu.Router) error     { return nil }
func (s *Service) Start(ctx context.Context) error { return nil }
func (s *Service) Stop(ctx context.Context) error  { return nil }
func (s *Service) Ready() error                    { return nil }
