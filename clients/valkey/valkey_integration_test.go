//go:build integration

package valkey_test

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	valkeygo "github.com/valkey-io/valkey-go"
	"go.opentelemetry.io/otel"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"log/slog"

	"github.com/techorionai/constellation-platform-go/clients/valkey"
	"github.com/techorionai/constellation-platform-go/config"
	"github.com/techorionai/constellation-platform-go/resources/test/containers"
)

func adminConfig(v *containers.ValkeyInstance, prefix string) valkey.Config {
	return valkey.Config{Address: v.Address(), Password: v.AdminPassword(), KeyPrefix: prefix}
}

func aclConfig(v *containers.ValkeyInstance, user string, password config.Secret, prefix string) valkey.Config {
	return valkey.Config{Address: v.Address(), Username: user, Password: password, KeyPrefix: prefix}
}

func do(ctx context.Context, c valkeygo.Client, args ...string) valkeygo.ValkeyResult {
	return c.Do(ctx, c.B().Arbitrary(args[0]).Args(args[1:]...).Build())
}

// I2
func TestIntegrationWarnsOnDefaultUser(t *testing.T) {
	v := containers.Valkey(t)
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })

	c, err := valkey.New(t.Context(), adminConfig(v, "identity"))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	got, err := c.Raw.Do(t.Context(), c.Raw.B().Ping().Build()).ToString()
	if err != nil || got != "PONG" {
		t.Fatalf("PING: %q %v", got, err)
	}
	out := buf.String()
	// The admin config has a password and no TLS, so New also writes the warn
	// record of finding R2 of the review of PR 5. Count the default user record.
	if strings.Count(out, "default user") != 1 || !strings.Contains(out, `"level":"WARN"`) {
		t.Fatalf("log output: %s", out)
	}
}

// I4
func TestIntegrationWrongPassword(t *testing.T) {
	v := containers.Valkey(t)
	admin := v.Admin(t)
	c, err := valkey.New(t.Context(), adminConfig(v, "identity"))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	bad := adminConfig(v, "identity")
	bad.Password = "wr0ng-pass"
	if _, err := valkey.New(t.Context(), bad); err == nil {
		t.Fatal("want error")
	} else if strings.Contains(err.Error(), "wr0ng-pass") {
		t.Fatalf("error has the password: %v", err)
	}

	deadline := time.Now().Add(time.Second)
	for {
		list, err := do(t.Context(), admin, "CLIENT", "LIST").ToString()
		if err != nil {
			t.Fatal(err)
		}
		n := len(strings.Split(strings.TrimSpace(list), "\n"))
		if n == 2 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("CLIENT LIST has %d connections, want 2:\n%s", n, list)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// I9
func TestIntegrationKeyPrefixRule(t *testing.T) {
	v := containers.Valkey(t)
	user, password := v.NewACLUser(t, "identity")
	c, err := valkey.New(t.Context(), aclConfig(v, user, password, "identity"))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := do(t.Context(), c.Raw, "SET", c.Keys.Key("a"), "1").Error(); err != nil {
		t.Fatalf("SET in prefix: %v", err)
	}
	err = do(t.Context(), c.Raw, "SET", "helpdesk:a", "1").Error()
	if err == nil || !strings.Contains(err.Error(), "NOPERM") {
		t.Fatalf("SET outside prefix: %v", err)
	}
}

// I13
func TestIntegrationConnectionMetrics(t *testing.T) {
	old := otel.GetMeterProvider()
	reader := sdkmetric.NewManualReader()
	otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)))
	t.Cleanup(func() { otel.SetMeterProvider(old) })

	v := containers.Valkey(t)
	c, err := valkey.New(t.Context(), adminConfig(v, "identity"))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.Raw.Do(t.Context(), c.Raw.B().Ping().Build()).Error(); err != nil {
		t.Fatal(err)
	}
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(t.Context(), &rm); err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	var success int64
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			names[m.Name] = true
			if s, ok := m.Data.(metricdata.Sum[int64]); ok && m.Name == "valkey_dial_success" {
				for _, dp := range s.DataPoints {
					success += dp.Value
				}
			}
		}
	}
	for _, n := range []string{"valkey_dial_attempt", "valkey_dial_success", "valkey_dial_conns", "valkey_dial_latency"} {
		if !names[n] {
			t.Errorf("metric %s not found in %v", n, names)
		}
	}
	if success < 1 {
		t.Errorf("valkey_dial_success = %d", success)
	}
}

// I14
func TestIntegrationDangerousCommandsRefused(t *testing.T) {
	v := containers.Valkey(t)
	admin := v.Admin(t)
	ctx := t.Context()
	user, password := v.NewACLUser(t, "identity")
	if err := do(ctx, admin, "SET", "helpdesk:a", "1").Error(); err != nil {
		t.Fatal(err)
	}
	lib := "#!lua name=adminlib\nredis.register_function('adminfn', function() return 1 end)"
	if err := do(ctx, admin, "FUNCTION", "LOAD", lib).Error(); err != nil {
		t.Fatal(err)
	}

	c, err := valkey.New(ctx, aclConfig(v, user, password, "identity"))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	newLib := "#!lua name=newlib\nredis.register_function('newfn', function() return 1 end)"
	commands := [][]string{
		{"FLUSHALL"},
		{"FLUSHDB"},
		{"KEYS", "*"},
		{"FUNCTION", "FLUSH"},
		{"FUNCTION", "LIST"},
		{"FUNCTION", "DELETE", "adminlib"},
		{"FUNCTION", "LOAD", newLib},
		{"SCRIPT", "FLUSH"},
	}
	for _, cmd := range commands {
		err := do(ctx, c.Raw, cmd...).Error()
		if err == nil || !strings.Contains(err.Error(), "NOPERM") {
			t.Errorf("%s: want NOPERM, got %v", strings.Join(cmd[:min(2, len(cmd))], " "), err)
		}
	}

	if got, err := do(ctx, admin, "GET", "helpdesk:a").ToString(); err != nil || got != "1" {
		t.Errorf("helpdesk:a = %q %v", got, err)
	}
	msgs, err := do(ctx, admin, "FUNCTION", "LIST").ToArray()
	if err != nil {
		t.Fatal(err)
	}
	text := ""
	for _, m := range msgs {
		text += m.String()
	}
	if !strings.Contains(text, "adminlib") || strings.Contains(text, "newlib") {
		t.Errorf("function libraries: %s", text)
	}
}

// I15
func TestIntegrationScanWithKeyPattern(t *testing.T) {
	v := containers.Valkey(t)
	admin := v.Admin(t)
	ctx := t.Context()
	user, password := v.NewACLUser(t, "identity")
	if err := do(ctx, admin, "SET", "helpdesk:a", "1").Error(); err != nil {
		t.Fatal(err)
	}
	c, err := valkey.New(ctx, aclConfig(v, user, password, "identity"))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	entry, err := do(ctx, c.Raw, "SCAN", "0", "MATCH", "*", "COUNT", "1000").ToArray()
	if err != nil {
		t.Fatalf("SCAN: %v", err)
	}
	keys, err := entry[1].AsStrSlice()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, k := range keys {
		if k == "helpdesk:a" {
			found = true
		}
	}
	t.Logf("I15: SCAN with a key pattern returns the key of another service: %v", found)
}
