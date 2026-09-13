package machine

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/cedar2025/xboard-node/internal/nlog"
	"github.com/cedar2025/xboard-node/internal/panel"
	relaypkg "github.com/cedar2025/xboard-node/internal/relay"
)

const (
	taskModeService = "service"
	taskModeRelay   = "relay"
)

func normalizeTaskMode(mode string) string {
	if strings.EqualFold(strings.TrimSpace(mode), taskModeRelay) {
		return taskModeRelay
	}
	return taskModeService
}

func resolveTaskMode(machineMode string, cfg *panel.NodeConfig) string {
	if cfg != nil && strings.TrimSpace(cfg.Mode) != "" {
		return normalizeTaskMode(cfg.Mode)
	}
	return normalizeTaskMode(machineMode)
}

func relayConfigFromPanel(cfg *panel.NodeConfig) (relaypkg.Config, error) {
	if cfg == nil || cfg.Relay == nil {
		return relaypkg.Config{}, fmt.Errorf("relay config missing")
	}
	idle := time.Duration(cfg.Relay.UDPIdleTimeout) * time.Second
	return relaypkg.Config{
		ListenIP:       cfg.Relay.ListenIP,
		ListenPort:     cfg.Relay.ListenPort,
		TargetHost:     cfg.Relay.TargetHost,
		TargetPort:     cfg.Relay.TargetPort,
		Networks:       cfg.Relay.Networks,
		UDPIdleTimeout: idle,
	}, nil
}

func (o *Orchestrator) runRelayNode(
	ctx context.Context,
	mn panel.MachineNode,
	client *panel.Client,
	initial *panel.NodeConfig,
	updates <-chan *panel.NodeConfig,
) error {
	initialCfg, err := relayConfigFromPanel(initial)
	if err != nil {
		return err
	}
	runner, err := relaypkg.New(initialCfg)
	if err != nil {
		return err
	}

	runErr := make(chan error, 1)
	go func() {
		runErr <- runner.Run(ctx)
	}()

	nlog.Core().Info("native relay started",
		"node_id", mn.ID,
		"listen", fmt.Sprintf("%s:%d", initialCfg.ListenIP, initialCfg.ListenPort),
		"target", fmt.Sprintf("%s:%d", initialCfg.TargetHost, initialCfg.TargetPort),
		"networks", strings.Join(initialCfg.Networks, ","))

	pollTicker := time.NewTicker(o.pullInterval)
	defer pollTicker.Stop()

	apply := func(next *panel.NodeConfig) error {
		if next == nil {
			return nil
		}
		if resolveTaskMode(mn.Mode, next) != taskModeRelay {
			go o.rediscover(o.runCtx)
			return nil
		}
		nextCfg, err := relayConfigFromPanel(next)
		if err != nil {
			return err
		}
		if err := runner.Update(nextCfg); err != nil {
			return err
		}
		nlog.Core().Info("native relay config updated",
			"node_id", mn.ID,
			"listen", fmt.Sprintf("%s:%d", nextCfg.ListenIP, nextCfg.ListenPort),
			"target", fmt.Sprintf("%s:%d", nextCfg.TargetHost, nextCfg.TargetPort),
			"networks", strings.Join(nextCfg.Networks, ","))
		return nil
	}

	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-runErr:
			return err
		case next := <-updates:
			if err := apply(next); err != nil {
				nlog.Core().Warn("native relay WS update rejected", "node_id", mn.ID, "error", err)
			}
		case <-pollTicker.C:
			next, err := client.GetConfig()
			if err != nil {
				nlog.Core().Warn("native relay config poll failed", "node_id", mn.ID, "error", err)
				continue
			}
			if err := apply(next); err != nil {
				nlog.Core().Warn("native relay polled config rejected", "node_id", mn.ID, "error", err)
			}
		}
	}
}
