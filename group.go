// Copyright (C) 2014 JT Olds
// See LICENSE for copying information

package whoauth2

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/spacemonkeygo/errors"
	"golang.org/x/net/context"
	"golang.org/x/oauth2"
	"gopkg.in/webhelp.v1/whcompat"
	"gopkg.in/webhelp.v1/wherr"
	"gopkg.in/webhelp.v1/whmux"
	"gopkg.in/webhelp.v1/whredir"
	"gopkg.in/webhelp.v1/whroute"
)

// ProviderGroup is an http.Handler that keeps track of authentication for
// multiple OAuth2 providers.
//
// Assuming OAuth2 providers have been configured for Facebook, Google,
// LinkedIn, and Github, ProviderGroup handles requests to the following paths:
//  * /all/logout
//  * /facebook/login
//  * /facebook/logout
//  * /facebook/_cb
//  * /google/login
//  * /google/logout
//  * /google/_cb
//  * /linkedin/login
//  * /linkedin/logout
//  * /linkedin/_cb
//  * /github/login
//  * /github/logout
//  * /github/_cb
//
// ProviderGroup will also return associated state to you about each OAuth2
// provider's state, in addition to a LoginRequired middleware and a Login
// URL generator.
type ProviderGroup struct {
	handlers       map[string]*ProviderHandler
	mux            whmux.Dir
	urls           RedirectURLs
	group_base_url string
}

// NewProviderGroup makes a provider group. Requires a session namespace (will
// be prepended to ":"+provider_name), the base URL of the ProviderGroup's
// http.Handler, a collection of URLs for redirecting, and a list of specific
// configured providers.
func NewProviderGroup(session_namespace string, group_base_url string,
	urls RedirectURLs, providers ...*Provider) (*ProviderGroup, error) {

	group_base_url = strings.TrimRight(group_base_url, "/")

	g := &ProviderGroup{
		handlers:       make(map[string]*ProviderHandler, len(providers)),
		urls:           urls,
		group_base_url: group_base_url}

	g.mux = whmux.Dir{
		"all": whmux.Dir{"logout": whmux.Exact(
			http.HandlerFunc(g.logoutAll))},
	}

	for _, provider := range providers {
		if provider.Name == "" {
			return nil, fmt.Errorf("empty provider name")
		}
		_, exists := g.handlers[provider.Name]
		if exists {
			return nil, fmt.Errorf("two providers given with name %#v",
				provider.Name)
		}
		handler := NewProviderHandler(provider,
			fmt.Sprintf("%s-%s", session_namespace, provider.Name),
			fmt.Sprintf("%s/%s", group_base_url, provider.Name), urls)
		g.handlers[provider.Name] = handler
		g.mux[provider.Name] = handler
	}

	return g, nil
}

func (g *ProviderGroup) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	g.mux.ServeHTTP(w, r)
}

// Routes implements whroute.Lister
func (g *ProviderGroup) Routes(
	cb func(method, path string, annotations map[string]string)) {
	whroute.Routes(g.mux, cb)
}

var _ http.Handler = (*ProviderGroup)(nil)
var _ whroute.Lister = (*ProviderGroup)(nil)

// Handler returns a specific ProviderHandler given the Provider name
func (g *ProviderGroup) Handler(provider_name string) (rv *ProviderHandler,
	exists bool) {
	rv, exists = g.handlers[provider_name]
	return rv, exists
}

// LoginURL returns the login URL for a given provider.
// redirect_to is the URL to navigate to after logging in, and force_prompt
// tells OAuth2 whether or not the login prompt should always be shown
// regardless of if the user is already logged in.
func (g *ProviderGroup) LoginURL(provider_name, redirect_to string,
	force_prompt bool) string {
	return g.handlers[provider_name].LoginURL(redirect_to, force_prompt)
}

// LogoutURL returns the logout URL for a given provider.
// redirect_to is the URL to navigate to after logging out.
func (g *ProviderGroup) LogoutURL(provider_name, redirect_to string) string {
	return g.handlers[provider_name].LogoutURL(redirect_to)
}

// LogoutAllURL returns the logout URL for all providers.
// redirect_to is the URL to navigate to after logging out.
func (g *ProviderGroup) LogoutAllURL(redirect_to string) string {
	return g.group_base_url + "/all/logout?" + url.Values{
		"redirect_to": {redirect_to}}.Encode()
}

// Tokens will return a map of all the currently valid OAuth2 tokens
func (g *ProviderGroup) Tokens(ctx context.Context) (map[string]*oauth2.Token,
	error) {
	rv := make(map[string]*oauth2.Token)
	var errs errors.ErrorGroup
	for name, handler := range g.handlers {
		token, err := handler.Token(ctx)
		errs.Add(err)
		if err == nil && token != nil {
			rv[name] = token
		}
	}
	return rv, errs.Finalize()
}

// Providers will return a map of all the currently known providers.
func (g *ProviderGroup) Providers() map[string]*ProviderHandler {
	copy := make(map[string]*ProviderHandler, len(g.handlers))
	for name, handler := range g.handlers {
		copy[name] = handler
	}
	return copy
}

// LoggedIn returns true if the user is logged in with any provider
func (g *ProviderGroup) LoggedIn(ctx context.Context) (bool, error) {
	t, err := g.Tokens(ctx)
	return len(t) > 0, err
}

// LogoutAll will not return any HTTP response, but will simply prepare a
// response for logging a user out completely from all providers. If a user
// should log out of just a specific OAuth2 provider, use the Logout method
// on the associated ProviderHandler.
func (g *ProviderGroup) LogoutAll(ctx context.Context,
	w http.ResponseWriter) error {
	var errs errors.ErrorGroup
	for _, handler := range g.handlers {
		errs.Add(handler.Logout(ctx, w))
	}
	return errs.Finalize()
}

func (g *ProviderGroup) logoutAll(w http.ResponseWriter, r *http.Request) {
	err := g.LogoutAll(whcompat.Context(r), w)
	if err != nil {
		wherr.Handle(w, r, err)
		return
	}
	redirect_to := r.FormValue("redirect_to")
	if redirect_to == "" {
		redirect_to = g.urls.DefaultLogoutURL
	}
	whredir.Redirect(w, r, redirect_to)
}

// LoginRequired is a middleware for redirecting users to a login page if
// they aren't logged in yet. login_redirect should take the URL to redirect
// to after logging in and return a URL that will actually do the logging in.
// If you already know which provider a user should use, consider using
// (*ProviderHandler).LoginRequired instead, which doesn't require a
// login_redirect URL.
func (g *ProviderGroup) LoginRequired(h http.Handler,
	login_redirect func(redirect_to string) (url string)) http.Handler {
	return whroute.HandlerFunc(h,
		func(w http.ResponseWriter, r *http.Request) {
			tokens, err := g.Tokens(whcompat.Context(r))
			if err != nil {
				wherr.Handle(w, r, err)
				return
			}
			if len(tokens) <= 0 {
				whredir.Redirect(w, r, login_redirect(r.RequestURI))
				return
			}
			h.ServeHTTP(w, r)
		})
}
