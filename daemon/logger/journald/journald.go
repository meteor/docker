// +build linux

// Package journald provides the log driver for forwarding server logs
// to endpoints that receive the systemd format.
package journald

import (
	"fmt"
	"strconv"
	"sync"
	"time"
	"unicode"

	"github.com/Sirupsen/logrus"
	"github.com/coreos/go-systemd/journal"
	"github.com/docker/docker/daemon/logger"
	"github.com/docker/docker/daemon/logger/loggerutils"
)

const name = "journald"

type journald struct {
	vars      map[string]string // additional variables and values to send to the journal along with the log message
	eVars     map[string]string // vars, plus an extra one saying DOCKER_EVENT=true
	readers   readerList
	rateLimit *rateLimit
}

type readerList struct {
	mu      sync.Mutex
	readers map[*logger.LogWatcher]*logger.LogWatcher
}

func init() {
	if err := logger.RegisterLogDriver(name, New); err != nil {
		logrus.Fatal(err)
	}
	if err := logger.RegisterLogOptValidator(name, validateLogOpt); err != nil {
		logrus.Fatal(err)
	}
}

// sanitizeKeyMode returns the sanitized string so that it could be used in journald.
// In journald log, there are special requirements for fields.
// Fields must be composed of uppercase letters, numbers, and underscores, but must
// not start with an underscore.
func sanitizeKeyMod(s string) string {
	n := ""
	for _, v := range s {
		if 'a' <= v && v <= 'z' {
			v = unicode.ToUpper(v)
		} else if ('Z' < v || v < 'A') && ('9' < v || v < '0') {
			v = '_'
		}
		// If (n == "" && v == '_'), then we will skip as this is the beginning with '_'
		if !(n == "" && v == '_') {
			n += string(v)
		}
	}
	return n
}

// Returns a rateLimit for the container if appropriate labels are set. Returns
// nil if labels are not set or cannot be parsed. Logs errors if labels cannot
// be parsed.
func newRateLimit(labels map[string]string) *rateLimit {
	burstLabel, burstExists := labels["com.meteor.galaxy.log-burst"]
	intervalLabel, intervalExists := labels["com.meteor.galaxy.log-interval"]

	if !burstExists && !intervalExists {
		return nil
	}
	if !burstExists || !intervalExists {
		logrus.Errorf("only one com.meteor.galaxy.log-* label exists: %v %v",
			burstExists, intervalExists)
		return nil
	}

	burst, err := strconv.Atoi(burstLabel)
	if err != nil {
		logrus.Errorf("Couldn't parse com.meteor.galaxy.log-burst '%s': %v",
			burstLabel, err)
		return nil
	}

	interval, err := time.ParseDuration(intervalLabel)
	if err != nil {
		logrus.Errorf("Couldn't parse com.meteor.galaxy.log-interval '%s': %v",
			intervalLabel, err)
		return nil
	}

	return &rateLimit{Burst: burst, Interval: interval}
}

// New creates a journald logger using the configuration passed in on
// the context.
func New(info logger.Info) (logger.Logger, error) {
	if !journal.Enabled() {
		return nil, fmt.Errorf("journald is not enabled on this host")
	}

	// parse log tag
	tag, err := loggerutils.ParseLogTag(info, loggerutils.DefaultTemplate)
	if err != nil {
		return nil, err
	}

	vars := map[string]string{
		"CONTAINER_ID":      info.ContainerID[:12],
		"CONTAINER_ID_FULL": info.ContainerID,
		"CONTAINER_NAME":    info.Name(),
		"CONTAINER_TAG":     tag,
	}
	extraAttrs, err := info.ExtraAttributes(sanitizeKeyMod)
	if err != nil {
		return nil, err
	}
	for k, v := range extraAttrs {
		vars[k] = v
	}

	eVars := map[string]string{"DOCKER_EVENT": "true"}
	for k, v := range vars {
		eVars[k] = v
	}

	return &journald{
		vars:      vars,
		eVars:     eVars,
		readers:   readerList{readers: make(map[*logger.LogWatcher]*logger.LogWatcher)},
		rateLimit: newRateLimit(ctx.ContainerLabels),
	}, nil
}

// We don't actually accept any options, but we have to supply a callback for
// the factory to pass the (probably empty) configuration map to.
func validateLogOpt(cfg map[string]string) error {
	for key := range cfg {
		switch key {
		case "labels":
		case "env":
		case "env-regex":
		case "tag":
		default:
			return fmt.Errorf("unknown log opt '%s' for journald log driver", key)
		}
	}
	return nil
}

func (s *journald) Log(msg *logger.Message) error {
	vars := map[string]string{}
	for k, v := range s.vars {
		vars[k] = v
	}
	if msg.Partial {
		vars["CONTAINER_PARTIAL_MESSAGE"] = "true"
	}

	line := string(msg.Line)
	source := msg.Source
	logger.PutMessage(msg)

	if source == "event" {
		// Galaxy-specific change! If this is an "event" (container start or stop),
		// send it with the special DOCKER_EVENT=true field. Also, use a distinct
		// priority level from stdout/stderr, since different priority levels are
		// rate limited separately by journald (though this is undocumented) and we
		// don't want a spammy container to cause journald to drop the stop message
		// if our internal rate limiting was ineffective.
		// https://github.com/systemd/systemd/blob/e5e0cffce784b2cf6f57f110cc9c4355f7703200/src/journal/journald-rate-limit.c#L39-L42
		return journal.Send(line, journal.PriWarning, s.eVars)
	}

	// If it's actually from the container, apply rate limiting. Note that we
	// don't rate limit stdout and stderr separately from each other.
	if s.rateLimit != nil {
		allowed, suppressed := s.rateLimit.Check()
		if !allowed {
			return nil
		}
		if suppressed > 0 {
			if err := s.sendSuppressedMessage(suppressed); err != nil {
				logrus.Errorf("Couldn't send suppressed message: %v", err)
			}
		}
	}

	if source == "stderr" {
		return journal.Send(line, journal.PriErr, vars)
	}
	return journal.Send(line, journal.PriInfo, vars)
}

// Send a DOCKER_EVENT message describing the suppression.
func (s *journald) sendSuppressedMessage(suppressed int) error {
	suppressedMessage := fmt.Sprintf(`{"type":"dropped","lines":%d}`, suppressed)
	return journal.Send(suppressedMessage, journal.PriWarning, s.eVars)
}

func (s *journald) Name() string {
	return name
}
