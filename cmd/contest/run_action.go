package main

import (
	"context"

	"github.com/docker/docker/api/types/container"
	"github.com/pkg/errors"
	"github.com/spikeekips/contest"
)

//revive:disable
func (cmd *runCommand) action(ctx context.Context, action contest.ScenarioAction) error { //nolint:gocognit //...
	mergeNodeArgs := func(args []string) ([]string, []string) {
		var nodeArgs []string

		switch i, found := cmd.vars.Value(".cmd.node_args"); {
		case !found:
		default:
			j, ok := i.([]string)
			if ok {
				nodeArgs = j
			}
		}

		args = append(args, nodeArgs...)

		return nodeArgs, args
	}

	switch action.Type {
	case "stop-contest":
		var err error

		if len(action.Args) > 0 {
			err = errors.New(action.Args[0])
		}

		cmd.exitch <- err

		return nil
	case "init-nodes":
		if err := cmd.rangeNodes(ctx, action,
			func(ctx context.Context, host contest.Host, alias string, args []string, _ map[string]interface{}) error {
				nodeArgs, args := mergeNodeArgs(args)

				log.Debug().
					Str("host", host.Address()).
					Str("alias", alias).
					Strs("args", args).
					Strs("node_args", nodeArgs).
					Msg("run init-nodes")

				return cmd.initNode(ctx, host, alias, args)
			}); err != nil {
			return errors.WithMessage(err, "init node")
		}
	case "run-nodes":
		if err := cmd.rangeNodes(ctx, action,
			func(ctx context.Context, host contest.Host, alias string, args []string, _ map[string]interface{}) error {
				nodeArgs, args := mergeNodeArgs(args)

				log.Debug().
					Str("host", host.Address()).
					Str("alias", alias).
					Strs("args", args).
					Strs("node_args", nodeArgs).
					Msg("run run-nodes")

				return cmd.runNode(ctx, host, alias, args)
			}); err != nil {
			return errors.WithMessage(err, "run node")
		}
	case "stop-nodes":
		if err := cmd.rangeNodes(ctx, action,
			func(ctx context.Context, host contest.Host, alias string, args []string, _ map[string]interface{}) error {
				log.Debug().
					Str("host", host.Address()).
					Str("alias", alias).
					Strs("args", args).
					Msg("run stop-nodes")

				_ = cmd.stopNodes(ctx, alias, nil)

				// NOTE ignore error

				return nil
			}); err != nil {
			return errors.WithMessage(err, "stop node")
		}
	case "host-command":
		if err := cmd.rangeNodes(ctx, action,
			func(_ context.Context, host contest.Host, alias string, args []string, _ map[string]interface{}) error {
				cmd, err := contest.LoadHostCommandArgs(args)
				if err != nil {
					return errors.WithStack(err)
				}

				log.Debug().
					Str("host", host.Address()).
					Str("alias", alias).
					Strs("args", args).
					Str("cmd", cmd).
					Msg("run host-command")

				switch _, _, ok, err := host.RunCommand(cmd); {
				case err != nil:
					return errors.WithStack(err)
				case !ok:
					return errors.Errorf("exit code != 0")
				default:
					return nil
				}
			},
		); err != nil {
			return errors.WithMessage(err, "run host command")
		}
	case "run-redis":
		err := cmd.hosts.TraverseByHost(func(h contest.Host, _ []string) (bool, error) {
			if err := cmd.startRedisContainer(ctx, h, func(body container.WaitResponse, err error) {
				if err != nil {
					cmd.exitch <- err

					return
				}

				if body.Error != nil {
					cmd.exitch <- errors.New(body.Error.Message)
				}
			}); err != nil {
				return false, err
			}

			return true, nil
		})
		if err != nil {
			return errors.WithMessage(err, "run redis")
		}
	case "run-nginx":
		hosts := map[string]contest.Host{}

		if err := cmd.rangeNodes(ctx, action,
			func(
				ctx context.Context,
				host contest.Host,
				_ string,
				_ []string,
				properties map[string]interface{},
			) error {
				if _, found := hosts[host.HostID()]; found {
					return nil
				}

				hosts[host.HostID()] = host

				return cmd.startNginxContainer(ctx, host, properties, func(body container.WaitResponse, err error) {
					if err != nil {
						cmd.exitch <- err

						return
					}

					if body.Error != nil {
						cmd.exitch <- errors.New(body.Error.Message)
					}
				})
			},
		); err != nil {
			return errors.WithMessage(err, "run host command")
		}
	}

	return nil
	//revive:enable
}
