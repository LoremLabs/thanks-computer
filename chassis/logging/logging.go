package logging

import (
	"regexp"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	"github.com/loremlabs/thanks-computer/chassis/config"
)

// Create Logger based on Environment / Runtime Config
func NewForConfig(config *config.Config) (*zap.Logger, error) {
	var zapConfig zap.Config

	// Set the logger
	zapConfig.EncoderConfig.MessageKey = "msg"
	zapConfig.EncoderConfig.LevelKey = "level"
	zapConfig.EncoderConfig.CallerKey = "caller"

	zapConfig.EncoderConfig.EncodeTime = zapcore.ISO8601TimeEncoder
	zapConfig.EncoderConfig.EncodeCaller = zapcore.ShortCallerEncoder

	var isDev = regexp.MustCompile(`^dev`)

	// Stack traces are off in dev (zap's dev config otherwise attaches
	// one to every WARN+, which buries operator-guidance lines like
	// the auth bootstrap banner). Prod keeps the default: stacktraces
	// at ERROR+ are useful for postmortems.
  switch config.Logger {
	case "dev":
		zapConfig = zap.NewDevelopmentConfig()
		zapConfig.DisableStacktrace = true
		zapConfig.DisableCaller = false
		zapConfig.EncoderConfig.EncodeTime = func(t time.Time, enc zapcore.PrimitiveArrayEncoder) {}
		zapConfig.EncoderConfig.EncodeLevel = zapcore.CapitalColorLevelEncoder
  case "dev-plain":
		zapConfig = zap.NewProductionConfig()
		zapConfig.DisableCaller = false
		zapConfig.DisableStacktrace = true
	case "production":
		zapConfig = zap.NewProductionConfig()
		zapConfig.DisableCaller = true
	default:
		switch {
		case isDev.MatchString(config.Environment):
			zapConfig = zap.NewDevelopmentConfig()
			zapConfig.DisableStacktrace = true
			zapConfig.DisableCaller = false
			zapConfig.EncoderConfig.EncodeTime = func(t time.Time, enc zapcore.PrimitiveArrayEncoder) {}
			zapConfig.EncoderConfig.EncodeLevel = zapcore.CapitalColorLevelEncoder
		default:
			zapConfig = zap.NewProductionConfig()
			zapConfig.DisableCaller = true
		}
	}

	switch config.LogLevel {
	case "trace":
		zapConfig.Level = zap.NewAtomicLevelAt(zap.DebugLevel)
	case "debug":
		zapConfig.Level = zap.NewAtomicLevelAt(zap.DebugLevel)
	case "info":
		zapConfig.Level = zap.NewAtomicLevelAt(zap.InfoLevel)
	case "warn":
		zapConfig.Level = zap.NewAtomicLevelAt(zap.WarnLevel)
	case "error":
		zapConfig.Level = zap.NewAtomicLevelAt(zap.ErrorLevel)
	case "fatal":
		zapConfig.Level = zap.NewAtomicLevelAt(zap.FatalLevel)
	default:
		switch {
		case isDev.MatchString(config.Environment):
			zapConfig.Level = zap.NewAtomicLevelAt(zap.DebugLevel)
		default:
			zapConfig.Level = zap.NewAtomicLevelAt(zap.InfoLevel)
		}
	}

	zapConfig.InitialFields = map[string]interface{}{
		"sid":  config.ServerId,
		"host": config.Fqdn,
		"env":  config.Environment,
	}

	logger, err := zapConfig.Build()
	if err != nil {
		return logger, err
	}

	return logger, nil
}
