// Package container runs services in Docker through testcontainers-go.
//
// testcontainers brings two things rig would otherwise have to write: wait
// strategies for deciding a container is ready, and the Ryuk reaper, which
// removes a stack's containers when the rig process dies without tearing
// down, even under SIGKILL.
package container

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"

	"github.com/rs/zerolog"
	"github.com/testcontainers/testcontainers-go"
	tclog "github.com/testcontainers/testcontainers-go/log"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/j0lvera/rig/internal/config"
	"github.com/j0lvera/rig/internal/stack"
)

type engine struct {
	logger zerolog.Logger

	setup    sync.Once
	setupErr error
}

var _ stack.Engine = (*engine)(nil)

// New returns an engine that runs services as Docker containers.
//
// testcontainers logs every pull, create, and wait to stderr, burying rig's
// own progress lines, so its logger is pointed at rig's debug log instead.
// That logger is package-level state, set here rather than per container:
// the per-container WithLogger option does not reach the Ryuk reaper, which
// logs through the package logger directly. Create one engine per process.
func New(logger zerolog.Logger) stack.Engine {
	tclog.SetDefault(debugLogger{logger: logger})

	return &engine{logger: logger}
}

// Start runs the service's image and waits for its ready check.
func (e *engine) Start(ctx context.Context, name string, svc config.Service, labels map[string]string) (stack.Container, error) {
	e.setup.Do(func() {
		set, err := configureDocker()
		for k, v := range set {
			e.logger.Debug().Str(k, v).Msg("docker configuration")
		}

		e.setupErr = err
	})

	if e.setupErr != nil {
		return nil, e.setupErr
	}

	port := strconv.Itoa(svc.Port) + "/tcp"

	opts := []testcontainers.ContainerCustomizer{
		testcontainers.WithExposedPorts(port),
		testcontainers.WithEnv(svc.Env),
		testcontainers.WithLabels(labels),
		testcontainers.WithWaitStrategyAndDeadline(svc.Timeout.Duration, strategies(svc, port)...),
	}

	if len(svc.Command) > 0 {
		opts = append(opts, testcontainers.WithCmd(svc.Command...))
	}

	ctr, err := testcontainers.Run(ctx, svc.Image, opts...)
	if err != nil {
		// A container that started but never became ready is still there.
		if ctr != nil {
			err = errors.Join(err, ctr.Terminate(context.WithoutCancel(ctx)))
		}

		return nil, fmt.Errorf("starting %s: %w", svc.Image, err)
	}

	c, err := wrap(ctx, ctr, port)
	if err != nil {
		return nil, errors.Join(err, ctr.Terminate(context.WithoutCancel(ctx)))
	}

	e.logger.Debug().Str("service", name).Str("id", c.id).Str("host", c.host).Int("port", c.port).
		Msg("service start")

	return c, nil
}

// strategies turns a ready check into wait strategies. The port check always
// applies: a log line can appear before the port is published to the host.
func strategies(svc config.Service, port string) []wait.Strategy {
	listening := wait.ForListeningPort(port)

	switch {
	case svc.Ready.Log != "":
		occurrences := max(svc.Ready.Occurrences, 1)
		return []wait.Strategy{wait.ForLog(svc.Ready.Log).AsRegexp().WithOccurrence(occurrences), listening}
	case svc.Ready.HTTP != "":
		return []wait.Strategy{listening, wait.ForHTTP(svc.Ready.HTTP).WithPort(port)}
	default:
		return []wait.Strategy{listening}
	}
}

type container struct {
	ctr  *testcontainers.DockerContainer
	id   string
	host string
	port int
}

var _ stack.Container = (*container)(nil)

func wrap(ctx context.Context, ctr *testcontainers.DockerContainer, port string) (*container, error) {
	host, err := ctr.Host(ctx)
	if err != nil {
		return nil, fmt.Errorf("reading container host: %w", err)
	}

	mapped, err := ctr.MappedPort(ctx, port)
	if err != nil {
		return nil, fmt.Errorf("reading mapped port: %w", err)
	}

	return &container{ctr: ctr, id: ctr.GetContainerID(), host: host, port: int(mapped.Num())}, nil
}

func (c *container) ID() string   { return c.id }
func (c *container) Host() string { return c.host }
func (c *container) Port() int    { return c.port }

func (c *container) Terminate(ctx context.Context) error {
	return c.ctr.Terminate(ctx)
}

// debugLogger adapts zerolog to testcontainers' Printf logger.
type debugLogger struct {
	logger zerolog.Logger
}

func (d debugLogger) Printf(format string, v ...any) {
	d.logger.Debug().Msgf(format, v...)
}
