// Copyright 2019 Canonical Ltd.
// Licensed under the AGPLv3, see LICENCE file for details.

package charmdownloader

import (
	"strings"

	"github.com/juju/errors"
	"gopkg.in/juju/worker.v1"
	"gopkg.in/juju/worker.v1/catacomb"

	"github.com/juju/juju/api/base"
	charmdownloadercli "github.com/juju/juju/api/charmdownloader"
)

// Config describes the necessary fields for NewWorker.
type Config struct {
	Logger    Logger
	APICaller base.APICaller
}

// Validate ensures all the necessary values are specified
func (c *Config) Validate() error {
	if c.Logger == nil {
		return errors.NotValidf("missing logger")
	}
	if c.APICaller == nil {
		return errors.NotValidf("missing API caller")
	}
	return nil
}

type charmDownloadWorker struct {
	config   Config
	catacomb catacomb.Catacomb
}

// NewWorker creates a new charmDownloadWorker, and starts an
// all model watcher.
func NewWorker(config Config) (worker.Worker, error) {
	if err := config.Validate(); err != nil {
		return nil, errors.Trace(err)
	}
	w := &charmDownloadWorker{
		config: config,
	}
	if err := catacomb.Invoke(catacomb.Plan{
		Site: &w.catacomb,
		Work: w.loop,
	}); err != nil {
		return nil, errors.Trace(err)
	}
	return w, nil
}

// Report returns information that is used in the dependency engine report.
func (w *charmDownloadWorker) Report() map[string]interface{} {
	return nil
}

func (w *charmDownloadWorker) loop() error {
	downloadFacade := charmdownloadercli.NewClient(w.config.APICaller)

	charmsWatcher, err := downloadFacade.WatchCharmsPendingForDownload()
	if err != nil {
		return err
	}
	if err := w.catacomb.Add(charmsWatcher); err != nil {
		return err
	}

	logger := w.config.Logger
	logger.Infof("started main loop")
	for {
		select {
		case <-w.catacomb.Dying():
			return nil
		case docs := <-charmsWatcher.Changes():
			if len(docs) == 0 {
				continue
			}

			// Strip model UUID from doc IDs so we can pass them
			// to the Download call
			for i, id := range docs {
				tokens := strings.SplitN(id, ":", 2)
				docs[i] = tokens[1]
			}

			if err := downloadFacade.Download(docs...); err != nil {
				return err
			}
		}
	}
}

// Kill is part of the worker.Worker interface.
func (w *charmDownloadWorker) Kill() {
	w.catacomb.Kill(nil)
}

// Wait is part of the worker.Worker interface.
func (w *charmDownloadWorker) Wait() error {
	return w.catacomb.Wait()
}
