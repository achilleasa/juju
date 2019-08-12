// Copyright 2019 Canonical Ltd.
// Licensed under the AGPLv3, see LICENCE file for details.

package charmdownloader

import (
	"github.com/juju/errors"
	"github.com/juju/juju/api/base"
	apiwatcher "github.com/juju/juju/api/watcher"
	"github.com/juju/juju/apiserver/params"
	"github.com/juju/juju/core/watcher"
)

type Client struct {
	base.ClientFacade
	facade    base.FacadeCaller
	apiCaller base.APICaller
}

// NewClient creates a new client for accessing the charms API.
func NewClient(st base.APICaller) *Client {
	frontend, backend := base.NewClientFacade(nopCloser{st}, "CharmDownloader")
	return &Client{
		ClientFacade: frontend,
		facade:       backend,
		apiCaller:    st,
	}
}

func (c *Client) Download(urls ...string) error {
	args := params.DownloadCharmsArgs{
		CharmsURLs: urls,
	}

	var res params.ErrorResults
	if err := c.facade.FacadeCall("Download", args, &res); err != nil {
		return errors.Trace(err)
	}
	return res.Combine()
}

func (c *Client) WatchCharmsPendingForDownload() (watcher.StringsWatcher, error) {
	var res params.StringsWatchResult
	if err := c.facade.FacadeCall("WatchCharmsPendingForDownload", nil, &res); err != nil {
		return nil, errors.Trace(err)
	}

	if res.Error != nil {
		return nil, errors.Trace(res.Error)
	}
	return apiwatcher.NewStringsWatcher(c.apiCaller, res), nil
}

type nopCloser struct {
	base.APICaller
}

func (nc nopCloser) Close() error { return nil }
