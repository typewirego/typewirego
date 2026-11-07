package typewirego_test

import (
	"context"
	"fmt"

	"github.com/typewirego/typewirego"
)

type Config struct{ DSN string }

type Store struct{ dsn string }

var configWire = typewirego.Of(typewirego.Opts[*Config]{
	Token: "app.Config",
	Creator: func(context.Context) (*Config, error) {
		return &Config{DSN: "postgres://localhost/app"}, nil
	},
})

var storeWire = typewirego.Of(typewirego.Opts[*Store]{
	Token:   "app.Store",
	Imports: []typewirego.Import{{Name: "config", Wire: configWire}},
	CreateWith: func(_ context.Context, deps typewirego.Deps, _ typewirego.Resolver) (*Store, error) {
		cfg, err := typewirego.Dep(deps, configWire)
		if err != nil {
			return nil, err
		}
		return &Store{dsn: cfg.DSN}, nil
	},
})

func ExampleOf() {
	type Config struct{ DSN string }
	type Store struct{ DSN string }

	// Wires are package-level values in a real service.
	configWire := typewirego.Of(typewirego.Opts[*Config]{
		Token: "app.Config",
		Creator: func(context.Context) (*Config, error) {
			return &Config{DSN: "postgres://localhost/app"}, nil
		},
	})
	storeWire := typewirego.Of(typewirego.Opts[*Store]{
		Token:   "app.Store",
		Imports: []typewirego.Import{{Name: "config", Wire: configWire}},
		CreateWith: func(_ context.Context, deps typewirego.Deps, _ typewirego.Resolver) (*Store, error) {
			cfg, err := typewirego.Dep(deps, configWire)
			if err != nil {
				return nil, err
			}
			return &Store{DSN: cfg.DSN}, nil
		},
	})

	ctx := context.Background()
	c := typewirego.NewContainer(ctx)
	defer c.Teardown(ctx)

	// Apply binds the store and, first, its import.
	if err := storeWire.Apply(c); err != nil {
		panic(err)
	}
	store, err := storeWire.GetInstance(ctx, c)
	if err != nil {
		panic(err)
	}
	again, _ := storeWire.GetInstance(ctx, c)
	fmt.Println(store.DSN)
	fmt.Println(store == again)
	// Output:
	// postgres://localhost/app
	// true
}

func ExampleTypeWire_WithCreator() {
	ctx := context.Background()
	c := typewirego.NewContainer(ctx)
	defer c.Teardown(ctx)

	// A test double for the config, applied before the wire that imports it,
	// so the store's Apply keeps it.
	fake := configWire.WithCreator(func(context.Context, typewirego.Deps, typewirego.Resolver, typewirego.Creator[*Config]) (*Config, error) {
		return &Config{DSN: "sqlite://memory"}, nil
	})
	_ = typewirego.GroupOf(fake, storeWire).Apply(c)

	store, _ := storeWire.GetInstance(ctx, c)
	fmt.Println(store.dsn)
	// Output: sqlite://memory
}

type conn struct{ name string }

func (c *conn) Close() { fmt.Println("closed", c.name) }

func ExampleContainer_Teardown() {
	ctx := context.Background()
	db := typewirego.Of(typewirego.Opts[*conn]{Token: "app.DB",
		Creator: func(context.Context) (*conn, error) { return &conn{name: "db"}, nil }})
	cache := typewirego.Of(typewirego.Opts[*conn]{Token: "app.Cache",
		Imports: []typewirego.Import{{Name: "db", Wire: db}},
		CreateWith: func(context.Context, typewirego.Deps, typewirego.Resolver) (*conn, error) {
			return &conn{name: "cache"}, nil
		}})

	c := typewirego.NewContainer(ctx)
	_ = cache.Apply(c)
	_, _ = cache.GetInstance(ctx, c)
	if err := c.Teardown(ctx); err != nil {
		fmt.Println(err)
	}
	// Output:
	// closed cache
	// closed db
}

func ExampleContainer_Graph() {
	ctx := context.Background()
	c := typewirego.NewContainer(ctx)
	defer c.Teardown(ctx)
	_ = storeWire.Apply(c)
	fmt.Print(c.Graph().DOT())
	// Output:
	// digraph wires {
	//   n1 [label="app.Config"]
	//   n2 [label="app.Store"]
	//   n1 -> n2
	// }
}
