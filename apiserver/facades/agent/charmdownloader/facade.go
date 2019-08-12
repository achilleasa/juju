// Copyright 2019 Canonical Ltd.
// Licensed under the AGPLv3, see LICENCE file for details.

package charmdownloader

import (
	"github.com/juju/errors"
	"github.com/juju/juju/apiserver/common"
	"github.com/juju/juju/apiserver/facade"
	"github.com/juju/juju/apiserver/facades/client/application"
	"github.com/juju/juju/apiserver/params"
	"github.com/juju/juju/permission"
	"github.com/juju/juju/state"
	"github.com/juju/loggo"
	charm "gopkg.in/juju/charm.v6"
	names "gopkg.in/juju/names.v2"
)

var logger = loggo.GetLogger("juju.apiserver.charmdownloader")

type backend interface {
	Charm(curl *charm.URL) (*state.Charm, error)
}

var getState = func(pool *state.StatePool, modelUUID string) (*state.PooledState, error) {
	st, err := pool.Get(modelUUID)
	if err != nil {
		return nil, err
	}

	return st, nil
}

type API struct {
	st         *state.State
	authorizer facade.Authorizer
}

func NewFacade(ctx facade.Context) (*API, error) {
	return &API{
		st:         ctx.State(),
		authorizer: ctx.Auth(),
	}, nil
}

func (a *API) Download(args params.DownloadCharmsArgs) params.ErrorResults {
	result := params.ErrorResults{
		Results: make([]params.ErrorResult, len(args.CharmsURLs)),
	}

	/*
		// FIXME: check permissions
		if err := a.checkIsAdmin(a.st.ModelUUID()); err != nil {
			result.Results[i].Error = common.ServerError(err)
			continue
		}
	*/

	for i, curl := range args.CharmsURLs {
		parsedURL, err := charm.ParseURL(curl)
		if err != nil {
			result.Results[i].Error = common.ServerError(err)
			continue
		}

		// Skip non-charmstore charms
		if parsedURL.Schema != "cs" {
			continue
		}

		// Skip already uploaded charms
		doc, err := a.st.Charm(parsedURL)
		if err != nil {
			result.Results[i].Error = common.ServerError(err)
			continue
		}

		if doc.IsUploaded() {
			continue
		}

		// Request the application facade to download the charm
		shim := application.NewStateShim(a.st)
		addCharmParams := params.AddCharmWithAuthorization{
			URL:           curl,
			Channel:       doc.Channel(),
			ForceDownload: true,
		}

		macaroon, err := doc.Macaroon()
		if err != nil {
			result.Results[i].Error = common.ServerError(err)
			continue
		}
		if macaroon != nil {
			addCharmParams.CharmStoreMacaroon = macaroon[0]
		}

		if err = application.AddCharmWithAuthorization(shim, addCharmParams); err != nil {
			result.Results[i].Error = common.ServerError(err)
			continue
		}
	}

	return result
}

func (a *API) checkIsAdmin(modelUUID string) error {
	isAdmin, err := a.authorizer.HasPermission(permission.AdminAccess, names.NewModelTag(modelUUID))
	if err != nil {
		return errors.Trace(err)
	}
	if !isAdmin {
		return common.ErrPerm
	}
	return nil
}
