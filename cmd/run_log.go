package main

import (
	"context"
	"io"
	"os"
	"path/filepath"

	dockerTypes "github.com/docker/docker/api/types"
	dockerstdcopy "github.com/docker/docker/pkg/stdcopy"
	"github.com/nxadm/tail"
	"github.com/pkg/errors"
	contest "github.com/spikeekips/contest2"
)

func (cmd *runCommand) saveContainerLogs(ctx context.Context, alias string) error {
	name := containerName(alias)

	host := cmd.hosts.HostByContainer(name)
	if host == nil {
		return errors.Errorf("host not found")
	}

	openfiles := func(fname string) (io.WriteCloser, *tail.Tail, error) {
		f, err := os.OpenFile(fname, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
		if err != nil {
			return nil, nil, err
		}

		t, err := tail.TailFile(fname, tail.Config{Follow: true})
		if err != nil {
			return nil, nil, err
		}

		return f, t, nil
	}

	logstdoutfilename := filepath.Join(cmd.basedir, alias+".stdout.log")
	logstderrfilename := filepath.Join(cmd.basedir, alias+".stderr.log")

	outf, outt, err := openfiles(logstdoutfilename)
	if err != nil {
		return err
	}

	errf, errt, err := openfiles(logstderrfilename)
	if err != nil {
		return err
	}

	r, err := host.ContainerLogs(ctx, name, dockerTypes.ContainerLogsOptions{
		ShowStdout: true, ShowStderr: true,
		Follow: true, Tail: "all",
	})
	if err != nil {
		return err
	}

	go func() {
		defer func() {
			_ = r.Close()
			_ = outf.Close()
			_ = errf.Close()
		}()

		if _, err := dockerstdcopy.StdCopy(outf, errf, r); err != nil {
			if !errors.Is(err, context.Canceled) {
				log.Debug().Err(err).Str("container", name).Msg("saving container logs stopped")
			}
		}
	}()

	savetail := func(t *tail.Tail, stderr bool) {
		for {
			select {
			case <-ctx.Done():
				return
			case l := <-t.Lines:
				if ctx.Err() != nil {
					break
				}

				if l.Err != nil {
					cmd.logch <- contest.NewInternalLogEntry("tail error", l.Err)
				}

				if len(l.Text) < 0 {
					continue
				}

				text := l.Text
				if e := log.Trace(); e.Enabled() {
					e.Str("node", alias).Str("text", text).Bool("stderr", stderr).Msg("new log text")
				}

				switch entry, err := contest.NewNodeLogEntry(alias, stderr, []byte(text)); {
				case err != nil:
					log.Error().Err(err).Str("node", alias).Str("text", text).Msg("wrong node log")
				default:
					cmd.logch <- entry
				}
			}
		}
	}

	go savetail(outt, false)
	go savetail(errt, true)

	return nil
}
