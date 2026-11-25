package clientcred

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"github.com/techorionai/constellation-platform-go/auth"
)

// The values are URIs of the standard, not credentials.
//
//nolint:gosec
const (
	grantTokenExchange = "urn:ietf:params:oauth:grant-type:token-exchange"
	tokenTypeAccessURI = "urn:ietf:params:oauth:token-type:access_token"
	tokenTypeAgentURI  = "urn:constellation:params:oauth:token-type:agent-id"

	exchangeCacheSize = 10000
	minExchangeExpiry = time.Second
	maxExchangeExpiry = 15 * time.Minute
)

type exchangeResponse struct {
	AccessToken     string  `json:"access_token"`
	IssuedTokenType string  `json:"issued_token_type"`
	TokenType       string  `json:"token_type"`
	ExpiresIn       float64 `json:"expires_in"`
	Error           string  `json:"error"`
}

// Exchange gets a token for the user, and for the agent that acts for the user
// if req.AgentID is not empty, with the token exchange grant (RFC 8693). It uses
// the cache when it can, and it does not retry. The client needs the permission
// identity:token:exchange and must be a service client with private_key_jwt.
//
// During the wait time of the source (see [Source.Token]), Exchange makes no
// HTTP call. It returns the unexpired cached token, or an [auth.UnavailableError].
//
// Errors: [auth.ErrInvalidToken] (empty subject token, or a value with a zero
// byte), [auth.ErrNoScopes], [auth.ErrBadTokenResponse],
// [auth.ErrTokenUnavailable] (see [auth.RetryAfter]), and *[auth.OAuthError]. Exchange never puts the
// subject token, the new token, or the agent ID in an error, a span, a log, or a
// metric.
func (s *Source) Exchange(ctx context.Context, req auth.ExchangeRequest) (string, error) {
	if req.SubjectToken == "" {
		return "", auth.ErrInvalidToken
	}
	scopes := normalize(req.Scopes)
	if len(scopes) == 0 {
		return "", auth.ErrNoScopes
	}
	for _, v := range append([]string{req.SubjectToken, req.AgentID, req.Audience}, scopes...) {
		if strings.ContainsRune(v, 0) {
			return "", fmt.Errorf("auth/clientcred: a value contains a zero byte: %w", auth.ErrInvalidToken)
		}
	}
	sum := sha256.Sum256([]byte(req.SubjectToken + "\x00" + req.AgentID + "\x00" + req.Audience + "\x00" + strings.Join(scopes, " ")))
	key := hex.EncodeToString(sum[:])
	if tok, ok := s.exchangeCached(key); ok {
		return tok, nil
	}
	if werr := s.waiting(); werr != nil {
		if old, ok := s.exchangeUnexpired(key); ok {
			return old, nil
		}
		return "", werr
	}
	// The shared request does not stop when one caller stops. The HTTP client
	// timeout limits it.
	ch := s.exchangeFlight.DoChan(key, func() (any, error) {
		if tok, ok := s.exchangeCached(key); ok {
			return tok, nil
		}
		if werr := s.waiting(); werr != nil {
			if old, ok := s.exchangeUnexpired(key); ok {
				return old, nil
			}
			return "", werr
		}
		tok, err := s.fetchExchange(context.WithoutCancel(ctx), key, req, scopes)
		if err != nil {
			// A failed refresh before the expiry keeps the cached token only when
			// the identity service is unavailable. An OAuth rejection or a bad
			// response removes the entry and goes to the caller.
			if errors.Is(err, auth.ErrTokenUnavailable) {
				if old, ok := s.exchangeUnexpired(key); ok {
					return old, nil
				}
			} else {
				s.exchangeCache.Delete(key)
			}
		}
		return tok, err
	})
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case r := <-ch:
		if r.Err != nil {
			return "", r.Err
		}
		return r.Val.(string), nil
	}
}

func (s *Source) exchangeCached(key string) (string, bool) {
	now := s.clock()
	e, ok := s.exchangeCache.Get(key, now)
	if !ok || !now.Before(e.refreshAt) {
		return "", false
	}
	return e.token, true
}

func (s *Source) exchangeUnexpired(key string) (string, bool) {
	e, ok := s.exchangeCache.Get(key, s.clock())
	if !ok {
		return "", false
	}
	return e.token, true
}

func (s *Source) fetchExchange(ctx context.Context, key string, req auth.ExchangeRequest, scopes []string) (string, error) {
	ctx, span := s.tracer.Start(ctx, "auth.clientcred.exchange", trace.WithAttributes(
		attribute.String("constellation.auth.client_id", s.cfg.ID),
		attribute.String("constellation.auth.audience", req.Audience),
		attribute.Bool("constellation.auth.exchange.with_agent", req.AgentID != "")))
	defer span.End()

	tok, outcome, err := s.requestExchange(ctx, key, req, scopes)
	s.exchanges.Add(ctx, 1, metric.WithAttributes(attribute.String("outcome", outcome)))
	span.SetAttributes(attribute.String("constellation.auth.outcome", outcome))
	if err != nil {
		span.SetStatus(codes.Error, outcome)
		return "", err
	}
	return tok, nil
}

func (s *Source) requestExchange(ctx context.Context, key string, req auth.ExchangeRequest, scopes []string) (token, outcome string, err error) {
	assertion, err := s.assertion()
	if err != nil {
		return "", outcomeBadResponse, errors.New("auth/clientcred: cannot sign the assertion")
	}
	form := url.Values{
		"grant_type":            {grantTokenExchange},
		"subject_token":         {req.SubjectToken},
		"subject_token_type":    {tokenTypeAccessURI},
		"requested_token_type":  {tokenTypeAccessURI},
		"audience":              {req.Audience},
		"scope":                 {strings.Join(scopes, " ")},
		"client_assertion_type": {assertionType},
		"client_assertion":      {assertion},
	}
	if req.AgentID != "" {
		form.Set("actor_token", req.AgentID)
		form.Set("actor_token_type", tokenTypeAgentURI)
	}
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, s.cfg.TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", outcomeBadResponse, errors.New("auth/clientcred: TokenURL is not valid")
	}
	hreq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	hreq.Header.Set("Accept", "application/json")
	start := s.clock()
	resp, err := s.client.Do(hreq)
	if err != nil {
		// A url.Error holds the URL. It holds no form value.
		return "", outcomeNetworkError, fmt.Errorf("%w: %w", auth.ErrTokenUnavailable, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return "", outcomeNetworkError, fmt.Errorf("%w: cannot read the response", auth.ErrTokenUnavailable)
	}
	if len(body) > maxResponseBytes {
		return "", outcomeBadResponse, fmt.Errorf("%w: body is too large", auth.ErrBadTokenResponse)
	}
	if rerr := s.rateLimited(resp); rerr != nil {
		return "", outcomeUnavailable, rerr
	}
	var tr exchangeResponse
	parsed := json.Unmarshal(body, &tr) == nil

	switch {
	case resp.StatusCode == http.StatusOK:
		switch {
		case !parsed || tr.AccessToken == "":
			return "", outcomeBadResponse, fmt.Errorf("%w: no access_token", auth.ErrBadTokenResponse)
		case tr.IssuedTokenType != tokenTypeAccessURI:
			return "", outcomeBadResponse, fmt.Errorf("%w: issued_token_type is not the access token type", auth.ErrBadTokenResponse)
		case tr.TokenType != "Bearer":
			return "", outcomeBadResponse, fmt.Errorf("%w: token_type is not Bearer", auth.ErrBadTokenResponse)
		case !(tr.ExpiresIn >= minExchangeExpiry.Seconds()) || tr.ExpiresIn > maxExchangeExpiry.Seconds():
			return "", outcomeBadResponse, fmt.Errorf("%w: expires_in is out of range", auth.ErrBadTokenResponse)
		}
		life := time.Duration(tr.ExpiresIn * float64(time.Second))
		s.exchangeCache.Set(key, entry{
			token:     tr.AccessToken,
			refreshAt: start.Add(time.Duration(float64(life) * refreshFraction)),
			expiresAt: start.Add(life),
		}, start.Add(life))
		return tr.AccessToken, outcomeOK, nil
	case resp.StatusCode >= 500 || (parsed && tr.Error == "temporarily_unavailable"):
		return "", outcomeUnavailable, fmt.Errorf("%w: status %d", auth.ErrTokenUnavailable, resp.StatusCode)
	case parsed && tr.Error != "":
		return "", outcomeOAuthError, &auth.OAuthError{Code: tr.Error}
	default:
		return "", outcomeBadResponse, fmt.Errorf("%w: status %d", auth.ErrBadTokenResponse, resp.StatusCode)
	}
}
