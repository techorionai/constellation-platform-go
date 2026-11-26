package jwt_test

import (
	"errors"
	"testing"
	"time"

	"github.com/techorionai/constellation-platform-go/auth"
	"github.com/techorionai/constellation-platform-go/resources/test/issuer"
)

// Issue 18 U1
func TestU1AmrAuthTime(t *testing.T) {
	f := newFixture(t)
	at := f.clock.Now().Add(-time.Minute).Truncate(time.Second)
	p := f.mustVerify(t, issuer.Claims{Extra: map[string]any{"amr": []string{"pwd", "otp", "mfa"}, "auth_time": at.Unix()}})
	if len(p.AuthMethods) != 3 || p.AuthMethods[0] != "pwd" || p.AuthMethods[2] != "mfa" {
		t.Errorf("amr %v", p.AuthMethods)
	}
	if !p.AuthTime.Equal(at) || !p.HasMFA() {
		t.Errorf("auth_time %v mfa %v", p.AuthTime, p.HasMFA())
	}
}

// Issue 18 U2
func TestU2AmrAbsentEmptyCustom(t *testing.T) {
	f := newFixture(t)
	for _, c := range []issuer.Claims{{}, {Extra: map[string]any{"amr": []string{}}}} {
		p := f.mustVerify(t, c)
		if len(p.AuthMethods) != 0 || !p.AuthTime.IsZero() || p.HasMFA() {
			t.Errorf("principal %+v", p)
		}
	}
	p := f.mustVerify(t, issuer.Claims{Extra: map[string]any{"amr": []string{"custom"}}})
	if len(p.AuthMethods) != 1 || p.AuthMethods[0] != "custom" || p.HasMFA() {
		t.Errorf("principal %+v", p)
	}
}

// Issue 18 U3
func TestU3AmrWrongType(t *testing.T) {
	f := newFixture(t)
	for _, v := range []any{"pwd", []any{1}, map[string]any{}, []any{nil}, 5, nil} {
		_, err := f.verify(t, issuer.Claims{Extra: map[string]any{"amr": v}})
		wantErr(t, err, auth.ErrInvalidToken)
	}
}

// Issue 18 U4
func TestU4AuthTimeFuture(t *testing.T) {
	f := newFixture(t)
	now := f.clock.Now()
	_, err := f.verify(t, issuer.Claims{Extra: map[string]any{"auth_time": now.Add(10 * time.Minute).Unix()}})
	wantErr(t, err, auth.ErrInvalidToken)
	f.mustVerify(t, issuer.Claims{Extra: map[string]any{"auth_time": now.Add(20 * time.Second).Unix()}})
}

// Issue 18 U5
func TestU5AuthTimeBad(t *testing.T) {
	f := newFixture(t)
	for _, v := range []any{"1700000000", 1e12} {
		_, err := f.verify(t, issuer.Claims{Extra: map[string]any{"auth_time": v}})
		wantErr(t, err, auth.ErrInvalidToken)
	}
}

// Issue 18 U24
func TestU24ActClearsMFA(t *testing.T) {
	f := newFixture(t)
	at := f.clock.Now().Add(-time.Minute).Unix()
	p := f.mustVerify(t, issuer.Claims{Extra: map[string]any{
		"amr": []string{"mfa"}, "auth_time": at,
		"act": map[string]any{"sub": "a1", "principal_type": "agent"},
	}})
	if p.Act == nil || len(p.AuthMethods) != 0 || !p.AuthTime.IsZero() || p.HasMFA() {
		t.Errorf("principal %+v", p)
	}
	if err := auth.CheckMFA(p, 5*time.Minute, f.clock.Now()); !errors.Is(err, auth.ErrReauthenticationRequired) {
		t.Errorf("err %v", err)
	}
}
