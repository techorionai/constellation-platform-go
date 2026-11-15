package config_test

import (
	"fmt"
	"os"

	"github.com/techorionai/constellation-platform-go/config"
)

func ExampleLoad() {
	_ = os.Setenv("IDENTITY_NAME", "identity")
	_ = os.Setenv("IDENTITY_DATABASE__URL", "postgres://u:p@localhost/db")
	defer func() {
		_ = os.Unsetenv("IDENTITY_NAME")
		_ = os.Unsetenv("IDENTITY_DATABASE__URL")
	}()

	type Database struct {
		URL config.Secret `env:"URL,required" envFile:"true"`
	}
	var cfg struct {
		Name     string   `env:"NAME,required"`
		Database Database `envPrefix:"DATABASE__"`
	}
	if err := config.Load("IDENTITY_", &cfg); err != nil {
		fmt.Println(err)
		return
	}
	fmt.Println(cfg.Name, cfg.Database.URL)
	// Output: identity [redacted]
}

func ExampleSecret() {
	s := config.Secret("password")
	fmt.Println(s)
	fmt.Println(s.Reveal())
	// Output:
	// [redacted]
	// password
}
