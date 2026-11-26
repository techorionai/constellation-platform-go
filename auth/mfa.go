package auth

import (
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"
)

// CheckMFA returns nil if the principal shows a fresh MFA login. Else it
// returns an error that wraps [ErrReauthenticationRequired].
//
// It refuses in this order: an API token, a principal with Act, a principal
// without "mfa" in AuthMethods, a negative maxAge, and an AuthTime that is zero
// or older than maxAge. The age limit is inclusive and has no leeway. With maxAge
// 0, CheckMFA does not check the age. The error text has the reason. Do not put
// it in a response body.
func CheckMFA(p Principal, maxAge time.Duration, now time.Time) error {
	switch {
	case p.TokenType.OrAccess() == TokenTypeAPI:
		return fmt.Errorf("%w: api token", ErrReauthenticationRequired)
	case p.Act != nil:
		return fmt.Errorf("%w: act", ErrReauthenticationRequired)
	case !p.HasMFA():
		return fmt.Errorf("%w: no mfa", ErrReauthenticationRequired)
	case maxAge < 0:
		return fmt.Errorf("%w: bad max age", ErrReauthenticationRequired)
	case maxAge > 0 && (p.AuthTime.IsZero() || now.Sub(p.AuthTime) > maxAge):
		return fmt.Errorf("%w: auth_time too old", ErrReauthenticationRequired)
	}
	return nil
}

// MFAChallenge returns the value of the WWW-Authenticate header of the refusal
// (RFC 9470 section 3). The max_age parameter is maxAge in whole seconds,
// rounded up. With maxAge 0 or less, the value has no max_age.
func MFAChallenge(maxAge time.Duration) string {
	v := `Bearer error="insufficient_user_authentication", error_description="A fresh MFA login is necessary"`
	if maxAge > 0 {
		secs := (maxAge + time.Second - 1) / time.Second
		v += `, max_age="` + strconv.FormatInt(int64(secs), 10) + `"`
	}
	return v
}

// WriteReauthenticationRequired writes the 401 refusal: the header
// WWW-Authenticate with [MFAChallenge] and a problem body with the code
// reauthentication_required.
func WriteReauthenticationRequired(w http.ResponseWriter, r *http.Request, maxAge time.Duration) {
	w.Header().Set("WWW-Authenticate", MFAChallenge(maxAge))
	writeBody(r.Context(), w, problem{
		Title: "Reauthentication required", Status: http.StatusUnauthorized, Code: "reauthentication_required",
	})
}

// RequireMFA returns the middleware that lets a request pass only if
// [CheckMFA] accepts the principal. Put it after [Middleware] and
// [RequireScopes]. It fails closed: a context with no principal gives a 500
// problem with the code auth_not_configured. It panics if maxAge is less than 0.
// It does not count in the metric constellation.auth.verify.
func RequireMFA(maxAge time.Duration) func(http.Handler) http.Handler {
	if maxAge < 0 {
		panic("auth: RequireMFA: maxAge is less than 0")
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := r.Context()
			p, ok := PrincipalFromContext(ctx)
			if !ok {
				slog.ErrorContext(ctx, "auth: RequireMFA has no principal, the chain has no auth.Middleware")
				writeBody(ctx, w, problem{Title: "Authentication not configured", Status: http.StatusInternalServerError, Code: "auth_not_configured"})
				return
			}
			if err := CheckMFA(p, maxAge, time.Now()); err != nil {
				WriteReauthenticationRequired(w, r, maxAge)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
