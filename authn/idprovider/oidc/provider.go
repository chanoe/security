/*
Copyright 2021 The tKeel Authors.
Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at
    http://www.apache.org/licenses/LICENSE-2.0
Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package oidc

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"net/http"

	"github.com/tkeel-io/security/authn/idprovider"

	"github.com/coreos/go-oidc"
	"github.com/golang-jwt/jwt"
	"golang.org/x/oauth2"
)

var _ idprovider.Provider = &OIDCProvider{}

const _oidcIdentityType string = "OIDCIdentityProvider"

type OIDCProvider struct {
	// Defines how Clients dynamically discover information about OpenID Providers
	// See also, https://openid.net/specs/openid-connect-discovery-1_0.html#ProviderConfig
	Issuer string `json:"issuer,omitempty" yaml:"issuer,omitempty"`

	// ClientID is the application's ID.
	ClientID string `json:"client_id" yaml:"clientID"` // nolint

	// ClientSecret is the application's secret.
	ClientSecret string `json:"-" yaml:"clientSecret"`

	// Endpoint contains the resource server's token endpoint URLs.
	// These are constants specific to each server and are often available via site-specific packages,
	// such as google.Endpoint or github.Endpoint.
	Endpoint endpoint `json:"endpoint" yaml:"endpoint"`

	// RedirectURL is the URL to redirect users going through
	// the OAuth flow, after the resource owner's URLs.
	RedirectURL string `json:"redirect_url" yaml:"redirectURL"` // nolint

	// Scope specifies optional requested permissions.
	Scopes []string `json:"scopes" yaml:"scopes"`

	// GetUserInfo uses the userinfo endpoint to get additional claims for the token.
	// This is especially useful where upstreams return "thin" id tokens
	// See also, https://openid.net/specs/openid-connect-core-1_0.html#UserInfo
	GetUserInfo bool `json:"get_user_info" yaml:"getUserInfo"`

	// Used to turn off TLS certificate checks.
	InsecureSkipVerify bool `json:"insecure_skip_verify" yaml:"insecureSkipVerify"`

	// Configurable key which contains the email claims.
	EmailKey string `json:"email_key" yaml:"emailKey"`

	// Configurable key which contains the preferred username claims.
	PreferredUsernameKey string `json:"preferred_username_key" yaml:"preferredUsernameKey"`

	Provider     *oidc.Provider        `json:"-" yaml:"-"`
	OAuth2Config *oauth2.Config        `json:"-" yaml:"-"`
	Verifier     *oidc.IDTokenVerifier `json:"-" yaml:"-"`
}

func (o *OIDCProvider) AuthCodeURL(state, nonce string) string {
	return o.OAuth2Config.AuthCodeURL(state, oidc.Nonce(nonce))
}

// endpoint represents an OAuth 2.0 provider's authorization and token
// endpoint URLs.
type endpoint struct {
	// URL of the OP's OAuth 2.0 Authorization Endpoint [OpenID.Core](https://openid.net/specs/openid-connect-discovery-1_0.html#OpenID.Core).
	AuthURL string `json:"auth_url" yaml:"authURL"` // nolint
	// URL of the OP's OAuth 2.0 Token Endpoint [OpenID.Core](https://openid.net/specs/openid-connect-discovery-1_0.html#OpenID.Core).
	// This is REQUIRED unless only the Implicit Flow is used.
	TokenURL string `json:"token_url" yaml:"tokenURL"` // nolint
	// URL of the OP's UserInfo Endpoint [OpenID.Core](https://openid.net/specs/openid-connect-discovery-1_0.html#OpenID.Core).
	// This URL MUST use the https scheme and MAY contain port, path, and query parameter components.
	UserInfoURL string `json:"user_info_url" yaml:"userInfoURL"` // nolint
	//  URL of the OP's JSON Web Key Set [JWK](https://openid.net/specs/openid-connect-discovery-1_0.html#JWK) document.
	JWKSURL string `json:"jwksurl"`
	// URL at the OP to which an RP can perform a redirect to request that the End-User be logged out at the OP.
	// This URL MUST use the https scheme and MAY contain port, path, and query parameter components.
	// https://openid.net/specs/openid-connect-rpinitiated-1_0.html#OPMetadata
	EndSessionURL string `json:"end_session_url"`
}

// nolint
func (o *OIDCProvider) AuthenticateCode(code string) (idprovider.Identity, error) {
	ctx := context.TODO()
	if o.InsecureSkipVerify {
		client := &http.Client{
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{
					InsecureSkipVerify: true, // nolint
				},
			},
		}
		ctx = context.WithValue(ctx, oauth2.HTTPClient, client)
	}
	token, err := o.OAuth2Config.Exchange(ctx, code)
	if err != nil {
		return nil, fmt.Errorf("oidc: failed to get token: %w", err)
	}
	rawIDToken, ok := token.Extra("id_token").(string)
	if !ok {
		return nil, errors.New("no id_token in token response")
	}
	var claims jwt.MapClaims
	if o.Verifier != nil {
		idToken, err := o.Verifier.Verify(ctx, rawIDToken)
		if err != nil {
			return nil, fmt.Errorf("failed to verify id token: %w", err)
		}
		if err := idToken.Claims(&claims); err != nil {
			return nil, fmt.Errorf("failed to decode id token claims: %w", err)
		}
	} else {
		_, _, err := new(jwt.Parser).ParseUnverified(rawIDToken, &claims)
		if err != nil {
			return nil, fmt.Errorf("failed to decode id token claims: %w", err)
		}
		if err := claims.Valid(); err != nil {
			return nil, fmt.Errorf("failed to verify id token: %w", err)
		}
	}
	if o.GetUserInfo {
		if o.Provider != nil {
			userInfo, err := o.Provider.UserInfo(ctx, oauth2.StaticTokenSource(token))
			if err != nil {
				return nil, fmt.Errorf("failed to fetch userinfo: %w", err)
			}
			if err := userInfo.Claims(&claims); err != nil {
				return nil, fmt.Errorf("failed to decode userinfo claims: %w", err)
			}
		} else {
			resp, err := oauth2.NewClient(ctx, oauth2.StaticTokenSource(token)).Get(o.Endpoint.UserInfoURL)
			if err != nil {
				return nil, fmt.Errorf("failed to fetch userinfo: %w", err)
			}
			data, err := ioutil.ReadAll(resp.Body)
			if err != nil {
				return nil, fmt.Errorf("failed to fetch userinfo: %w", err)
			}
			_ = resp.Body.Close()
			if err := json.Unmarshal(data, &claims); err != nil {
				return nil, fmt.Errorf("failed to decode userinfo claims: %w", err)
			}
		}
	}

	subject, ok := claims["sub"].(string)
	if !ok {
		return nil, errors.New("missing required claim \"sub\"")
	}

	var email string
	emailKey := "email"
	if o.EmailKey != "" {
		emailKey = o.EmailKey
	}
	email, _ = claims[emailKey].(string)

	var preferredUsername string
	preferredUsernameKey := "preferred_username"
	if o.PreferredUsernameKey != "" {
		preferredUsernameKey = o.PreferredUsernameKey
	}

	preferredUsername, _ = claims[preferredUsernameKey].(string)
	if preferredUsername == "" {
		preferredUsername, _ = claims["name"].(string)
	}

	return &oidcIdentity{
		Sub:               subject,
		PreferredUsername: preferredUsername,
		Email:             email,
	}, nil
	// todo  creat in internal user.
}

//nolint
func (o *OIDCProvider) Authenticate(username string, password string) (idprovider.Identity, error) {
	return nil, errors.New("unsupported authenticate with username password")
}

func (o *OIDCProvider) Type() string {
	return _oidcIdentityType
}
