package main

import (
	"context"
	"debug/elf"
	"path/filepath"
	"time"

	"github.com/pkg/errors"
	contest "github.com/spikeekips/contest2"
	"github.com/spikeekips/mitum/util"
	"go.mongodb.org/mongo-driver/bson"
)

var (
	contestID         = util.ULID().String()
	DefaultNodeImage  = "debian:testing-slim"
	DefaultRedisImage = "redis:latest"
)

var defaultMongodbURI = "mongodb://localhost:27017/contest_" + contestID

type runCommand struct {
	BaseDir      string        `arg:"" name:"base_directory" help:"base directory"`
	Design       string        `arg:"" name:"scenario" help:"scenario file" type:"existingfile"`
	Hosts        []HostFlag    `arg:"" name:"host" help:"docker host"`
	NodeBinaries []string      `name:"node-binary" help:"node binary files by architecture"`
	Mongodb      string        `name:"mongodb" help:"mongodb uri" default:"${mongodb_uri}"`
	Timeout      time.Duration `name:"timeout" help:"stop after timeout"`
	db           *contest.Mongodb
	basedir      string
	design       contest.Design
	vars         *contest.Vars
	hosts        *contest.Hosts
	logch        chan contest.LogEntry
	nodeBinaries map[elf.Machine]string
	exitch       chan error
}

func (cmd *runCommand) Run() error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := cmd.prepare(); err != nil {
		return err
	}

	cmd.exitch = make(chan error)

	defer func() {
		err := cmd.closeHosts()
		if err != nil {
			log.Error().Err(err).Msg("failed to close hosts")
		}
	}()

	cmd.logch = make(chan contest.LogEntry)

	w := contest.NewWatchLogs(
		cmd.design.Expects,
		cmd.logch,
		nil,
		cmd.vars,
		func(id string) contest.Host {
			return cmd.hosts.HostByContainer(containerName(id))
		},
		func(ctx context.Context, m bson.M) (interface{}, bool, error) {
			return cmd.db.Find(ctx, m)
		},
		cmd.action,
		cmd.db.InsertLogEntries,
	)

	_ = w.SetLogging(logging)

	go func() {
		cmd.exitch <- <-w.Wait(ctx)
	}()

	cmd.logch <- contest.NewInternalLogEntry("contest ready", nil)

	select {
	case <-func() <-chan time.Time {
		if cmd.Timeout < 1 {
			return nil
		}

		return time.After(cmd.Timeout)
	}():
		cancel()

		log.Debug().Dur("timeout", cmd.Timeout).Msg("contest will be stopped by timeout")

		return errors.Errorf("timeout after %s", cmd.Timeout)
	case err := <-cmd.exitch:
		cancel()

		log.Debug().Err(err).Msg("contest will be stopped by exit chan")

		if err != nil {
			return err
		}
	}

	return nil
}

func (cmd *runCommand) closeHosts() error {
	log.Debug().Msg("trying to close hosts")
	defer log.Debug().Msg("hosts closed")

	_ = cmd.hosts.Traverse(func(host contest.Host) (bool, error) {
		log.Debug().Str("host", host.HostID()).Msg("trying to collect result")
		defer log.Debug().Str("host", host.HostID()).Msg("collected result")

		_ = host.CollectResult(filepath.Join(cmd.basedir, host.Hostname()+"-"+filepath.Base(host.Base())+".tar.gz"))

		return true, nil
	})

	switch {
	case cmd.hosts == nil:
		return nil
	case cmd.hosts.Len() == 1:
		if err := cmd.hosts.Close(); err != nil {
			return err
		}
	}

	worker := util.NewErrgroupWorker(context.Background(), int64(cmd.hosts.Len()))
	defer worker.Close()

	_ = cmd.hosts.Traverse(func(host contest.Host) (bool, error) {
		if err := worker.NewJob(func(context.Context, uint64) error {
			_ = host.Close()

			return nil
		}); err != nil {
			return false, err
		}

		return true, nil
	})

	worker.Done()

	return worker.Wait()
}
