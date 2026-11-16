package cache_test

import (
	"context"
	"fmt"
	"time"

	"github.com/techorionai/constellation-platform-go/cache"
	"github.com/techorionai/constellation-platform-go/resources/test/mock"
)

type Profile struct {
	Name string `json:"name"`
}

func ExampleCache_GetOrLoad() {
	// A service uses the Valkey adapter. This sample uses the mock store.
	var store cache.Store = &mock.CacheStore{}
	profiles := cache.New[Profile](store, "profile")

	load := func(context.Context) (Profile, error) {
		fmt.Println("load")
		return Profile{Name: "Ada"}, nil
	}
	for range 2 {
		p, err := profiles.GetOrLoad(context.Background(), "user:42", time.Minute, load)
		fmt.Println(p.Name, err)
	}
	// Output:
	// load
	// Ada <nil>
	// Ada <nil>
}

func ExampleCache_Get() {
	profiles := cache.New[Profile](&mock.CacheStore{}, "profile")
	_, err := profiles.Get(context.Background(), "user:42")
	fmt.Println(err)
	// Output: cache: miss
}

func ExampleInstrument() {
	store := cache.Instrument(&mock.CacheStore{}, "raw")
	err := store.Set(context.Background(), "k", []byte("v"), time.Minute)
	fmt.Println(err)
	// Output: <nil>
}
