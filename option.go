package cron

import (
	"time"
)

// Option represents a modification to the default behavior of a Cron.
type Option func(*Cron)

// WithLocation overrides the timezone of the cron instance.
func WithLocation(loc *time.Location) Option {
	return func(c *Cron) {
		c.location = loc
	}
}

// WithSeconds overrides the parser used for interpreting job schedules to
// include a seconds field as the first one.
func WithSeconds() Option {
	return WithParser(NewParser(
		Second | Minute | Hour | Dom | Month | Dow | Descriptor,
	))
}

// WithParser overrides the parser used for interpreting job schedules.
func WithParser(p ScheduleParser) Option {
	return func(c *Cron) {
		c.parser = p
	}
}

// WithChain specifies Job wrappers to apply to all jobs added to this cron.
// Refer to the Chain* functions in this package for provided wrappers.
func WithChain(wrappers ...JobWrapper) Option {
	return func(c *Cron) {
		c.chain = NewChain(wrappers...)
	}
}

// WithLogger uses the provided logger.
func WithLogger(logger Logger) Option {
	return func(c *Cron) {
		c.logger = logger
	}
}

// WithDSTPolicy sets the default policies for handling daylight-saving
// transitions on every SpecSchedule added to this cron (including schedules
// using CRON_TZ=/TZ= prefixes). Schedules whose policies were already set
// explicitly keep their own values. Schedules with fixed-interval semantics,
// such as those produced by "@every", are unaffected.
func WithDSTPolicy(repeated RepeatedPolicy, skipped SkippedPolicy) Option {
	return func(c *Cron) {
		c.repeatedPolicy = repeated
		c.skippedPolicy = skipped
	}
}
