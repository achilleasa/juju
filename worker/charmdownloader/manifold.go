// Copyright 2019 Canonical Ltd.
// Licensed under the AGPLv3, see LICENCE file for details.

package charmdownloader

import (
	"github.com/juju/errors"
	"gopkg.in/juju/worker.v1"
	"gopkg.in/juju/worker.v1/dependency"

	"github.com/juju/juju/api/base"
)

// Logger describes the logging methods used in this package by the worker.
type Logger interface {
	Infof(string, ...interface{})
	Errorf(string, ...interface{})
}

// ManifoldConfig holds the information necessary to run a charmdownloader
// worker in a dependency.Engine.
type ManifoldConfig struct {
	APICallerName string
	Logger        Logger

	NewWorker func(Config) (worker.Worker, error)
}

// Validate validates the manifold configuration.
func (config ManifoldConfig) Validate() error {
	if config.APICallerName == "" {
		return errors.NotValidf("empty APICallerName")
	}
	if config.Logger == nil {
		return errors.NotValidf("missing Logger")
	}
	if config.NewWorker == nil {
		return errors.NotValidf("missing NewWorker func")
	}
	return nil
}

// Manifold returns a dependency.Manifold that will run a charmdownloader
// worker.
func Manifold(config ManifoldConfig) dependency.Manifold {
	return dependency.Manifold{
		Inputs: []string{
			config.APICallerName,
		},
		Start: config.start,
	}
}

func (config ManifoldConfig) start(context dependency.Context) (worker.Worker, error) {
	if err := config.Validate(); err != nil {
		return nil, errors.Trace(err)
	}
	var apiCaller base.APICaller
	if err := context.Get(config.APICallerName, &apiCaller); err != nil {
		return nil, errors.Trace(err)
	}

	w, err := config.NewWorker(Config{
		Logger:    config.Logger,
		APICaller: apiCaller,
	})
	if err != nil {
		return nil, errors.Trace(err)
	}
	return w, nil
}
