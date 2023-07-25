package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"sync"

	dockerTypes "github.com/docker/docker/api/types"
	dockerstdcopy "github.com/docker/docker/pkg/stdcopy"
	"github.com/pkg/errors"
	"github.com/spikeekips/contest"
	"github.com/spikeekips/mitum/util"
)

type logFile struct {
	stdout io.WriteCloser
	stderr io.WriteCloser
}

func (cmd *runCommand) newLogFile(_ context.Context, alias string) (io.WriteCloser, io.WriteCloser, error) {
	lf, _, _, err := cmd.logFiles.GetOrCreate(alias, func() (*logFile, error) {
		outfname := filepath.Join(cmd.basedir, alias+".stdout.log")
		errfname := filepath.Join(cmd.basedir, alias+".stderr.log")

		outf, err := os.OpenFile(outfname, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
		if err != nil {
			return nil, errors.WithStack(err)
		}

		errf, err := os.OpenFile(errfname, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
		if err != nil {
			return nil, errors.WithStack(err)
		}

		return &logFile{
			stdout: &containerLogFile{
				logch: cmd.logch,
				f:     outf,
				alias: alias,
			},
			stderr: &containerLogFile{
				logch:    cmd.logch,
				f:        errf,
				alias:    alias,
				isstderr: true,
			},
		}, nil
	})
	if err != nil {
		return nil, nil, err
	}

	return lf.stdout, lf.stderr, nil
}

func (cmd *runCommand) saveContainerLogs(ctx context.Context, alias string) error {
	name := containerName(alias)

	host := cmd.hosts.HostByContainer(name)
	if host == nil {
		return errors.Errorf("host not found")
	}

	outf, errf, err := cmd.newLogFile(ctx, alias)
	if err != nil {
		return err
	}

	r, err := host.ContainerLogs(ctx, name, dockerTypes.ContainerLogsOptions{
		ShowStdout: true, ShowStderr: true,
		Follow: true, Tail: "all",
	})
	if err != nil {
		return errors.WithStack(err)
	}

	go func() {
		if _, err := dockerstdcopy.StdCopy(outf, errf, r); err != nil {
			if !errors.Is(err, context.Canceled) {
				log.Debug().Err(err).Str("container", name).Msg("saving container logs stopped")
			}
		}

		_ = r.Close()
	}()

	return nil
}

type containerLogFile struct {
	f     io.WriteCloser
	logch chan contest.LogEntry
	alias string
	rem   []byte
	sync.Mutex
	isstderr bool
}

func (f *containerLogFile) Write(b []byte) (int, error) {
	if len(b) < 1 {
		return 0, nil
	}

	f.Lock()
	defer f.Unlock()

	n, err := f.f.Write(b)
	if err != nil {
		return n, errors.WithStack(err)
	}

	left, _ := contest.BytesLines(b, func(l []byte) error {
		if len(f.rem) > 0 {
			l = util.ConcatBytesSlice(f.rem, l)

			f.rem = nil
		}

		if e := log.Trace(); e.Enabled() {
			e.Str("node", f.alias).Str("text", string(l)).Bool("stderr", f.isstderr).Msg("new log text")
		}

		if len(l) > 0 {
			switch entry, err := contest.NewNodeLogEntry(f.alias, f.isstderr, l); {
			case err != nil:
				log.Error().Err(err).Str("node", f.alias).Str("text", string(l)).Msg("wrong log")
			default:
				f.logch <- entry
			}
		}

		return nil
	})

	f.rem = util.ConcatBytesSlice(f.rem, left)

	return n, nil
}

func (f *containerLogFile) Close() error {
	f.Lock()
	defer f.Unlock()

	return errors.WithStack(f.f.Close())
}
