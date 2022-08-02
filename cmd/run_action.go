package main

import (
	"context"

	"github.com/docker/docker/api/types/container"
	"github.com/pkg/errors"
	contest "github.com/spikeekips/contest2"
)

func (cmd *runCommand) action(ctx context.Context, action contest.ScenarioAction) error {
	switch action.Type {
	case "run-nodes":
		if err := cmd.rangeNodes(ctx, action,
			func(ctx context.Context, host contest.Host, alias string, args []string) error {
				log.Debug().
					Str("host", host.Address()).
					Str("alias", alias).
					Strs("args", args).
					Msg("run run-nodes")

				return cmd.runNode(ctx, host, alias, args)
			}); err != nil {
			return errors.WithMessage(err, "failed to run node")
		}
	case "stop-nodes":
		if err := cmd.rangeNodes(ctx, action,
			func(ctx context.Context, host contest.Host, alias string, args []string) error {
				log.Debug().
					Str("host", host.Address()).
					Str("alias", alias).
					Strs("args", args).
					Msg("run stop-nodes")

				_ = cmd.stopNodes(ctx, alias, nil)

				// NOTE ignore error

				return nil
			}); err != nil {
			return errors.WithMessage(err, "failed to stop node")
		}
	case "host-command":
		if err := cmd.rangeNodes(ctx, action,
			func(ctx context.Context, host contest.Host, alias string, args []string) error {
				cmd, err := contest.LoadHostCommandArgs(args)
				if err != nil {
					return err
				}

				log.Debug().
					Str("host", host.Address()).
					Str("alias", alias).
					Strs("args", args).
					Str("cmd", cmd).
					Msg("run host-command")

				switch _, _, ok, err := host.RunCommand(cmd); {
				case err != nil:
					return err
				case !ok:
					return errors.Errorf("exit code != 0")
				default:
					return nil
				}
			},
		); err != nil {
			return errors.WithMessage(err, "failed to run host command")
		}
	case "run-redis":
		err := cmd.hosts.TraverseByHost(func(h contest.Host, _ []string) (bool, error) {
			if err := cmd.startRedisContainer(ctx, h, func(body container.ContainerWaitOKBody, err error) {
				if err != nil {
					cmd.exitch <- err

					return
				}

				if body.Error != nil {
					cmd.exitch <- errors.Errorf(body.Error.Message)
				}
			}); err != nil {
				return false, err
			}

			return true, nil
		})
		if err != nil {
			return errors.WithMessage(err, "failed to run redis")
		}
	}

	return nil
}
