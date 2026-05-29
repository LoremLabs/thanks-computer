package registry

import (
	"go.uber.org/zap"

	"github.com/loremlabs/thanks-computer/chassis/config"
)

type Registry struct {
	Conf   config.Config
	Logger *zap.Logger
	Name   string `json:"name,omitempty"`
}

// New Create a Registry Object
func New(conf config.Config, logger *zap.Logger) Registry {
	return Registry{Conf: conf, Logger: logger}
}
