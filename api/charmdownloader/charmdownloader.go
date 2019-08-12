// Copyright 2019 Canonical Ltd.
// Licensed under the AGPLv3, see LICENCE file for details.

package charmdownloader

import (
	"github.com/juju/errors"
	"github.com/juju/juju/api/base"
	"github.com/juju/juju/apiserver/params"
)

type Client struct {
	base.ClientFacade
	facade base.FacadeCaller
}

// NewClient creates a new client for accessing the charms API.
func NewClient(st base.APICaller) *Client {
	frontend, backend := base.NewClientFacade(nopCloser{st}, "CharmDownloader")
	return &Client{ClientFacade: frontend, facade: backend}
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

type nopCloser struct {
	base.APICaller
}

func (nc nopCloser) Close() error { return nil }
