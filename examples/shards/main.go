// A configured set of instances: the members are not known until the config
// is read, so no type can name them. They are registered as a group, folded
// into a registry, and picked by a value the resolving scope provides. Every
// constructor here is plain, so the whole graph is checked before anything
// is built: nothing is unchecked and only the shard name is owed.
package main

import (
	"context"
	"fmt"

	"github.com/floatdrop/di"
)

type DB struct{ dsn string }

func (d *DB) Query() string { return "query " + d.dsn }

type Config struct{ Shards []string }

type Shard struct {
	Name string
	DB   *DB
}

type Shards map[string]*DB

type ShardName string

func selectShard(want ShardName, all Shards) (*DB, error) {
	db, ok := all[string(want)]
	if !ok {
		return nil, fmt.Errorf("no shard %q", want)
	}
	return db, nil
}

// The fold is an ordinary constructor too: its parameter is the group, which
// Needs says at the registration rather than here.
func shardRegistry(all []Shard) Shards {
	m := Shards{}
	for _, sh := range all {
		m[sh.Name] = sh.DB
	}
	return m
}

func main() {
	cfg := Config{Shards: []string{"eu-1", "us-1"}}

	app := di.New()
	app.Value(cfg)

	// One binding per configured shard, read back together as a registry.
	for _, name := range cfg.Shards {
		app.Value(Shard{Name: name, DB: &DB{dsn: name}}).Group()
	}
	app.Wire[Shards](shardRegistry).Needs(di.AllOf[Shard]())

	// The selector is an ordinary constructor: its ShardName parameter is the
	// key, and Scoped() leaves the choice to the scope that resolves it.
	app.Wire[*DB](selectShard).Scoped()

	for _, tenant := range cfg.Shards {
		req := app.Child(tenant)
		req.Value(ShardName(tenant))
		fmt.Println(tenant, "->", req.Get[*DB]().Query())
		_ = req.Stop(context.Background())
	}

	// The root does not provide ShardName, so Validate reports it as owed by
	// whichever scope resolves the shard rather than as a failure.
	fmt.Println("owed:", app.Validate().Owed)

	_ = app.Stop(context.Background())
}
