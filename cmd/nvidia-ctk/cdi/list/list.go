/**
# Copyright (c) 2022, NVIDIA CORPORATION.  All rights reserved.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.
**/

package list

import (
	"context"
	"errors"
	"fmt"

	"github.com/urfave/cli/v3"
	"tags.cncf.io/container-device-interface/pkg/cdi"

	"github.com/NVIDIA/nvidia-container-toolkit/internal/logger"
)

type command struct {
	logger logger.Interface
}

type config struct {
	cdiSpecDirs []string
}

// NewCommand constructs a cdi list command with the specified logger
func NewCommand(logger logger.Interface) *cli.Command {
	c := command{
		logger: logger,
	}
	return c.build()
}

// build creates the CLI command
func (m command) build() *cli.Command {
	cfg := config{}

	// Create the command
	c := cli.Command{
		Name:  "list",
		Usage: "List the available CDI devices",
		Before: func(ctx context.Context, cmd *cli.Command) (context.Context, error) {
			return ctx, m.validateFlags(&cfg)
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			return m.run(&cfg)
		},
		Flags: []cli.Flag{
			&cli.StringSliceFlag{
				Name:        "spec-dir",
				Usage:       "specify the directories to scan for CDI specifications",
				Value:       cdi.DefaultSpecDirs,
				Destination: &cfg.cdiSpecDirs,
				Sources:     cli.EnvVars("NVIDIA_CTK_CDI_SPEC_DIRS"),
			},
		},
	}

	return &c
}

func (m command) validateFlags(cfg *config) error {
	if len(cfg.cdiSpecDirs) == 0 {
		return errors.New("at least one CDI specification directory must be specified")
	}
	return nil
}

func (m command) run(cfg *config) error {
	registry, err := cdi.NewCache(
		cdi.WithAutoRefresh(false),
		cdi.WithSpecDirs(cfg.cdiSpecDirs...),
	)
	if err != nil {
		return fmt.Errorf("failed to create CDI cache: %v", err)
	}

	_ = registry.Refresh()
	if errors := registry.GetErrors(); len(errors) > 0 {
		m.logger.Warningf("The following registry errors were reported:")
		for k, err := range errors {
			m.logger.Warningf("%v: %v", k, err)
		}
	}

	devices := registry.ListDevices()
	m.logger.Infof("Found %d CDI devices", len(devices))
	for _, device := range devices {
		fmt.Printf("%s\n", device)
	}

	return nil
}
