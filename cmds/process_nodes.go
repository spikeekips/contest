package cmds

import (
	"context"
	"fmt"

	"github.com/spikeekips/mitum/launch/pm"
	"github.com/spikeekips/mitum/util/logging"
	"golang.org/x/xerrors"

	"github.com/spikeekips/contest/config"
	"github.com/spikeekips/contest/host"
)

const ProcessNameNodes = "nodes"

var ProcessorNodes pm.Process

func init() {
	if i, err := pm.NewProcess(
		ProcessNameNodes,
		[]string{ProcessNameConfig, ProcessNameHosts},
		ProcessNodes,
	); err != nil {
		panic(err)
	} else {
		ProcessorNodes = i
	}
}

func ProcessNodes(ctx context.Context) (context.Context, error) {
	var log logging.Logger
	if err := config.LoadLogContextValue(ctx, &log); err != nil {
		return ctx, err
	}

	var design config.Design
	if err := config.LoadDesignContextValue(ctx, &design); err != nil {
		return ctx, err
	}

	var logDir string
	if err := config.LoadLogDirContextValue(ctx, &logDir); err != nil {
		return ctx, err
	}

	var hosts *host.Hosts
	if err := host.LoadHostsContextValue(ctx, &hosts); err != nil {
		return ctx, err
	}

	var vars *config.Vars
	if err := config.LoadVarsContextValue(ctx, &vars); err != nil {
		return nil, err
	}

	log.Debug().Msg("trying to prepare hosts")
	var nodesConfig map[string][]byte
	if b, err := generateNodesConfig(ctx, design, hosts); err != nil {
		return ctx, xerrors.Errorf("failed to generate nodes config: %w", err)
	} else {
		nodesConfig = b
	}

	if err := hosts.TraverseNodes(func(node *host.Node) (bool, error) {
		vars.Set(fmt.Sprintf("Design.Node.%s", node.Alias()), node.ConfigMap())

		if err := saveNodeConfig(node.Alias(), logDir, node.ConfigData(), nodesConfig[node.Alias()]); err != nil {
			return false, err
		} else {
			return true, nil
		}
	}); err != nil {
		return ctx, err
	}

	log.Debug().Msg("hosts and nodes prepared")

	return context.WithValue(ctx, config.ContextValueVars, vars), nil
}
