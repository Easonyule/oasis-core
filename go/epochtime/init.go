// Package epochtime implements the Oasis timekeeping backend.
package epochtime

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"

	"github.com/oasislabs/ekiden/go/epochtime/api"
	"github.com/oasislabs/ekiden/go/epochtime/mock"
	"github.com/oasislabs/ekiden/go/epochtime/system"
	"github.com/oasislabs/ekiden/go/epochtime/tendermint"
	"github.com/oasislabs/ekiden/go/tendermint/service"
)

const (
	cfgBackend            = "epochtime.backend"
	cfgSystemInterval     = "epochtime.system.interval"
	cfgTendermintInterval = "epochtime.tendermint.interval"
)

var (
	flagBackend            string
	flagSystemInterval     int64
	flagTendermintInterval int64
)

// New constructs a new Backend based on the configuration flags.
func New(cmd *cobra.Command, tmService service.TendermintService) (api.Backend, error) {
	backend, _ := cmd.Flags().GetString(cfgBackend)
	switch strings.ToLower(backend) {
	case system.BackendName:
		interval, _ := cmd.Flags().GetInt64(cfgSystemInterval)
		return system.New(interval)
	case mock.BackendName:
		return mock.New(), nil
	case tendermint.BackendName:
		interval, _ := cmd.Flags().GetInt64(cfgTendermintInterval)
		return tendermint.New(tmService, interval)
	default:
		return nil, fmt.Errorf("epochtime: unsupported backend: '%v'", backend)
	}
}

// RegisterFlags registers the configuration flags with the provided
// command.
func RegisterFlags(cmd *cobra.Command) {
	cmd.Flags().StringVar(&flagBackend, cfgBackend, system.BackendName, "Epoch time backend")
	cmd.Flags().Int64Var(&flagSystemInterval, cfgSystemInterval, api.EpochInterval, "Epoch interval")
	cmd.Flags().Int64Var(&flagTendermintInterval, cfgTendermintInterval, api.EpochInterval, "Epoch interval (in blocks)")

	for _, v := range []string{
		cfgBackend,
		cfgSystemInterval,
		cfgTendermintInterval,
	} {
		viper.BindPFlag(v, cmd.Flags().Lookup(v)) //nolint: errcheck
	}
}
