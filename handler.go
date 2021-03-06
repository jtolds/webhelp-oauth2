// Copyright (C) 2014 JT Olds
// See LICENSE for copying information

package whoauth2 // import "gopkg.in/go-webhelp/whoauth2.v1"

import (
	"encoding/gob"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"golang.org/x/net/context"
	"golang.org/x/oauth2"
	"gopkg.in/webhelp.v1/whcompat"
	"gopkg.in/webhelp.v1/wherr"
	"gopkg.in/webhelp.v1/whmux"
	"gopkg.in/webhelp.v1/whredir"
	"gopkg.in/webhelp.v1/whroute"
	"gopkg.in/webhelp.v1/whsess"
)

func init() {
	gob.Register(&oauth2.Token{})
}

// ProviderHandler is an http.Handler that keeps track of authentication for
// a single OAuth2 provider
//
// ProviderHandler handles requests to the following paths:
//  * /login
//  * /logout
//  * /_cb
//
// ProviderHandler will also return associated state to you about its state,
// in addition to a LoginRequired middleware and a Login URL generator.
type ProviderHandler struct {
	provider          *Provider
	session_namespace string
	handler_base_url  string
	urls              RedirectURLs
	accessOffline     bool
	whmux.Dir
}

// NewProviderHandler makes a provider handler. Requires a provider
// configuration, a session namespace, a base URL for the handler, and a
// collection of URLs for redirecting.
func NewProviderHandler(provider *Provider, session_namespace string,
	handler_base_url string, urls RedirectURLs) *ProviderHandler {
	if urls.DefaultLoginURL == "" {
		urls.DefaultLoginURL = "/"
	}
	if urls.DefaultLogoutURL == "" {
		urls.DefaultLogoutURL = "/"
	}
	h := &ProviderHandler{
		provider:          provider,
		session_namespace: session_namespace,
		handler_base_url:  strings.TrimRight(handler_base_url, "/"),
		urls:              urls}
	h.Dir = whmux.Dir{
		"login":  whmux.Exact(http.HandlerFunc(h.login)),
		"logout": whmux.Exact(http.HandlerFunc(h.logout)),
		"_cb":    whmux.Exact(http.HandlerFunc(h.cb))}
	return h
}

// RequestOfflineTokens tells the provider to request oauth2 tokens with
// AccessTypeOffline instead of AccessTypeOnline.
func (o *ProviderHandler) RequestOfflineTokens() {
	o.accessOffline = true
}

// Token returns a token if the provider is currently logged in, or nil if not.
func (o *ProviderHandler) Token(ctx context.Context) (*oauth2.Token, error) {
	session, err := o.Session(ctx)
	if err != nil {
		return nil, err
	}
	return o.token(session), nil
}

func (o *ProviderHandler) Provider() *Provider { return o.provider }

// Session returns a provider-specific authenticated session for the current
// user. This session is cleared whenever a user logs out.
func (o *ProviderHandler) Session(ctx context.Context) (*whsess.Session,
	error) {
	return whsess.Load(ctx, o.session_namespace)
}

// LoggedIn returns true if the user is logged in with this provider
func (o *ProviderHandler) LoggedIn(ctx context.Context) (bool, error) {
	t, err := o.Token(ctx)
	return t != nil, err
}

func (o *ProviderHandler) token(session *whsess.Session) *oauth2.Token {
	val, exists := session.Values["_token"]
	token, correct := val.(*oauth2.Token)
	if exists && correct && token.Valid() {
		return token
	}
	return nil
}

// Logout prepares the request to log the user out of just this OAuth2
// provider. If you're using a ProviderGroup you may be interested in
// LogoutAll.
func (o *ProviderHandler) Logout(ctx context.Context,
	w http.ResponseWriter) error {
	session, err := o.Session(ctx)
	if err != nil {
		return err
	}
	return session.Clear(ctx, w)
}

// LoginURL returns the login URL for this provider
// redirect_to is the URL to navigate to after logging in, and force_prompt
// tells OAuth2 whether or not the login prompt should always be shown
// regardless of if the user is already logged in.
func (o *ProviderHandler) LoginURL(redirect_to string,
	force_prompt bool) string {
	return o.handler_base_url + "/login?" + url.Values{
		"redirect_to":  {redirect_to},
		"force_prompt": {fmt.Sprint(force_prompt)}}.Encode()
}

// LogoutURL returns the logout URL for this provider
// redirect_to is the URL to navigate to after logging out.
func (o *ProviderHandler) LogoutURL(redirect_to string) string {
	return o.handler_base_url + "/logout?" + url.Values{
		"redirect_to": {redirect_to}}.Encode()
}

func (o *ProviderHandler) login(w http.ResponseWriter, r *http.Request) {
	ctx := whcompat.Context(r)
	session, err := o.Session(ctx)
	if err != nil {
		wherr.Handle(w, r, err)
		return
	}

	redirect_to := r.FormValue("redirect_to")
	if redirect_to == "" {
		redirect_to = o.urls.DefaultLoginURL
	}

	force_prompt, err := strconv.ParseBool(r.FormValue("force_prompt"))
	if err != nil {
		force_prompt = false
	}

	if !force_prompt && o.token(session) != nil {
		whredir.Redirect(w, r, redirect_to)
		return
	}

	state := newState()
	session.Values["_state"] = state
	session.Values["_redirect_to"] = redirect_to
	err = session.Save(ctx, w)
	if err != nil {
		wherr.Handle(w, r, err)
		return
	}

	opts := make([]oauth2.AuthCodeOption, 0, 2)
	if o.accessOffline {
		opts = append(opts, oauth2.AccessTypeOffline)
	} else {
		opts = append(opts, oauth2.AccessTypeOnline)
	}
	if force_prompt {
		opts = append(opts, oauth2.ApprovalForce)
	}

	whredir.Redirect(w, r, o.provider.AuthCodeURL(state, opts...))
}

func (o *ProviderHandler) cb(w http.ResponseWriter, r *http.Request) {
	ctx := whcompat.Context(r)
	session, err := o.Session(ctx)
	if err != nil {
		wherr.Handle(w, r, err)
		return
	}

	val, exists := session.Values["_state"]
	existing_state, correct := val.(string)
	if !exists || !correct {
		wherr.Handle(w, r,
			wherr.BadRequest.New("invalid session storage state"))
		return
	}

	val, exists = session.Values["_redirect_to"]
	redirect_to, correct := val.(string)
	if !exists || !correct {
		wherr.Handle(w, r,
			wherr.BadRequest.New("invalid redirect_to"))
		return
	}

	if existing_state != r.FormValue("state") {
		wherr.Handle(w, r, wherr.BadRequest.New("csrf detected"))
		return
	}

	var accessType oauth2.AuthCodeOption
	if o.accessOffline {
		accessType = oauth2.AccessTypeOffline
	} else {
		accessType = oauth2.AccessTypeOnline
	}

	token, err := o.provider.Exchange(ctx, r.FormValue("code"), accessType)
	if err != nil {
		wherr.Handle(w, r, err)
		return
	}

	session.Values["_token"] = token
	err = session.Save(ctx, w)
	if err != nil {
		wherr.Handle(w, r, err)
		return
	}

	whredir.Redirect(w, r, redirect_to)
}

func (o *ProviderHandler) logout(w http.ResponseWriter, r *http.Request) {
	err := o.Logout(whcompat.Context(r), w)
	if err != nil {
		wherr.Handle(w, r, err)
		return
	}
	redirect_to := r.FormValue("redirect_to")
	if redirect_to == "" {
		redirect_to = o.urls.DefaultLogoutURL
	}
	whredir.Redirect(w, r, redirect_to)
}

func (o *ProviderHandler) loginRequired(h http.Handler, forcePrompt bool) http.Handler {
	return whroute.HandlerFunc(h,
		func(w http.ResponseWriter, r *http.Request) {
			token, err := o.Token(whcompat.Context(r))
			if err != nil {
				wherr.Handle(w, r, err)
				return
			}
			if token == nil {
				whredir.Redirect(w, r, o.LoginURL(r.RequestURI, forcePrompt))
				return
			}
			h.ServeHTTP(w, r)
		})
}

// LoginRequired is a middleware for redirecting users to a login page if
// they aren't logged in yet. If you are using a ProviderGroup and don't know
// which provider a user should use, consider using
// (*ProviderGroup).LoginRequired instead
func (o *ProviderHandler) LoginRequired(h http.Handler) http.Handler {
	return o.loginRequired(h, false)
}

// LoginRequiredForcePrompt is a middleware for redirecting users to a login page if
// they aren't logged in yet. If you are using a ProviderGroup and don't know
// which provider a user should use, consider using
// (*ProviderGroup).LoginRequired instead.
func (o *ProviderHandler) LoginRequiredForcePrompt(h http.Handler) http.Handler {
	return o.loginRequired(h, true)
}
