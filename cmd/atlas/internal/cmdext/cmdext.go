// Copyright 2021-present The Atlas Authors. All rights reserved.
// This source code is licensed under the Apache 2.0 license found
// in the LICENSE file in the root directory of this source tree.

// Package cmdext provides extensions to the Atlas CLI that
// may be moved to a separate repository in the future.
package cmdext

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"strconv"
	"time"

	"ariga.io/atlas/schemahcl"
	"ariga.io/atlas/sql/migrate"
	"ariga.io/atlas/sql/schema"
	"ariga.io/atlas/sql/sqlclient"

	"entgo.io/ent/dialect/sql"
	entschema "entgo.io/ent/dialect/sql/schema"
	"entgo.io/ent/entc"
	"entgo.io/ent/entc/gen"
	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/gohcl"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/zclconf/go-cty/cty"
	"gocloud.dev/runtimevar"
	_ "gocloud.dev/runtimevar/awssecretsmanager"
	_ "gocloud.dev/runtimevar/constantvar"
	_ "gocloud.dev/runtimevar/filevar"
	_ "gocloud.dev/runtimevar/gcpsecretmanager"
	_ "gocloud.dev/runtimevar/httpvar"
)

// DataSources exposes the data sources provided by this package.
var DataSources = []schemahcl.Option{
	schemahcl.WithDataSource("sql", QuerySrc),
	schemahcl.WithDataSource("runtimevar", RuntimeVarSrc),
}

// RuntimeVarSrc exposes the gocloud.dev/runtimevar as a schemahcl datasource.
//
//	data "runtimevar" "pass" {
//	  url = "driver://path?query=param"
//	}
//
//	locals {
//	  url = "mysql://root:${data.runtimevar.pass}@:3306/"
//	}
func RuntimeVarSrc(c *hcl.EvalContext, block *hclsyntax.Block) (cty.Value, error) {
	var (
		args struct {
			URL string `hcl:"url"`
		}
		ctx    = context.Background()
		errorf = blockError("data.runtimevar", block)
	)
	if diags := gohcl.DecodeBody(block.Body, c, &args); diags.HasErrors() {
		return cty.NilVal, errorf("decoding body: %v", diags)
	}
	u, err := url.Parse(args.URL)
	if err != nil {
		return cty.NilVal, errorf("parsing url: %v", err)
	}
	if d := u.Query().Get("decoder"); d != "" && d != "string" {
		return cty.NilVal, errorf("unsupported decoder: %q", d)
	}
	q := u.Query()
	q.Set("decoder", "string")
	// Default timeout is 10s unless specified otherwise.
	timeout := 10 * time.Second
	if t := q.Get("timeout"); t != "" {
		if timeout, err = time.ParseDuration(t); err != nil {
			return cty.NilVal, errorf("parsing timeout: %v", err)
		}
		q.Del("timeout")
	}
	u.RawQuery = q.Encode()
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	vr, err := runtimevar.OpenVariable(ctx, u.String())
	if err != nil {
		return cty.Value{}, errorf("opening variable: %v", err)
	}
	defer vr.Close()
	snap, err := vr.Latest(ctx)
	if err != nil {
		return cty.Value{}, errorf("getting latest snapshot: %v", err)
	}
	sv, ok := snap.Value.(string)
	if !ok {
		return cty.Value{}, errorf("unexpected snapshot value type: %T", snap.Value)
	}
	return cty.StringVal(sv), nil
}

// QuerySrc exposes the database/sql.Query as a schemahcl datasource.
//
//	data "sql" "tenants" {
//	  url = var.url
//	  query = <query>
//	  args = [<arg1>, <arg2>, ...]
//	}
//
//	env "prod" {
//	  for_each = toset(data.sql.tenants.values)
//	  url      = urlsetpath(var.url, each.value)
//	}
func QuerySrc(ctx *hcl.EvalContext, block *hclsyntax.Block) (cty.Value, error) {
	var (
		args struct {
			URL    string   `hcl:"url"`
			Query  string   `hcl:"query"`
			Remain hcl.Body `hcl:",remain"`
			Args   []any
		}
		values []cty.Value
		errorf = blockError("data.sql", block)
	)
	if diags := gohcl.DecodeBody(block.Body, ctx, &args); diags.HasErrors() {
		return cty.NilVal, errorf("decoding body: %v", diags)
	}
	attrs, diags := args.Remain.JustAttributes()
	if diags.HasErrors() {
		return cty.NilVal, errorf("getting attributes: %v", diags)
	}
	if at, ok := attrs["args"]; ok {
		switch v, diags := at.Expr.Value(ctx); {
		case diags.HasErrors():
			return cty.NilVal, errorf(`evaluating "args": %w`, diags)
		case !v.CanIterateElements():
			return cty.NilVal, errorf(`attribute "args" must be a list, got: %s`, v.Type())
		default:
			for it := v.ElementIterator(); it.Next(); {
				switch _, v := it.Element(); v.Type() {
				case cty.String:
					args.Args = append(args.Args, v.AsString())
				case cty.Number:
					f, _ := v.AsBigFloat().Float64()
					args.Args = append(args.Args, f)
				case cty.Bool:
					args.Args = append(args.Args, v.True())
				default:
					return cty.NilVal, errorf(`attribute "args" must be a list of strings, numbers or booleans, got: %s`, v.Type())
				}
			}
		}
		delete(attrs, "args")
	}
	if len(attrs) > 0 {
		return cty.NilVal, errorf("unexpected attributes: %v", attrs)
	}
	c, err := sqlclient.Open(context.Background(), args.URL)
	if err != nil {
		return cty.NilVal, errorf("opening connection: %w", err)
	}
	defer c.Close()
	rows, err := c.QueryContext(context.Background(), args.Query, args.Args...)
	if err != nil {
		return cty.NilVal, errorf("executing query: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var v any
		if err := rows.Scan(&v); err != nil {
			return cty.NilVal, errorf("scanning row: %w", err)
		}
		switch v := v.(type) {
		case bool:
			values = append(values, cty.BoolVal(v))
		case int64:
			values = append(values, cty.NumberIntVal(v))
		case float64:
			values = append(values, cty.NumberFloatVal(v))
		case string:
			values = append(values, cty.StringVal(v))
		case []byte:
			values = append(values, cty.StringVal(string(v)))
		default:
			return cty.NilVal, errorf("unsupported row type: %T", v)
		}
	}
	obj := map[string]cty.Value{
		"count":  cty.NumberIntVal(int64(len(values))),
		"values": cty.ListValEmpty(cty.NilType),
		"value":  cty.NilVal,
	}
	if len(values) > 0 {
		obj["value"] = values[0]
		obj["values"] = cty.ListVal(values)
	}
	return cty.ObjectVal(obj), nil
}

func blockError(name string, b *hclsyntax.Block) func(string, ...any) error {
	return func(format string, args ...any) error {
		return fmt.Errorf("%s.%s: %w", name, b.Labels[1], fmt.Errorf(format, args...))
	}
}

type (
	// LoadStateOptions for external state loaders.
	LoadStateOptions struct {
		URLs []*url.URL
		Dev  *sqlclient.Client // Client for the dev database.
	}
	// StateLoader allows loading StateReader's from external sources.
	StateLoader interface {
		LoadState(context.Context, *LoadStateOptions) (migrate.StateReader, error)
	}

	// MigrateDiffOptions for external migration differ.
	MigrateDiffOptions struct {
		Name string
		To   []string
		Dir  migrate.Dir
		Dev  *sqlclient.Client
	}
	// MigrateDiffer allows external sources to implement custom migration differs.
	MigrateDiffer interface {
		MigrateDiff(context.Context, *MigrateDiffOptions) error
		needDiff([]string) bool
	}
)

// States is a global registry for external state loaders.
var States = registry{
	"ent": EntLoader{},
}

type registry map[string]StateLoader

// Loader returns the state loader for the given scheme.
func (r registry) Loader(scheme string) (StateLoader, bool) {
	l, ok := r[scheme]
	return l, ok
}

// Differ returns the raw states differ for the given URLs, if registered.
func (r registry) Differ(to []string) (MigrateDiffer, bool) {
	for _, l := range r {
		if d, ok := l.(MigrateDiffer); ok && d.needDiff(to) {
			return d, true
		}
	}
	return nil, false
}

// EntLoader is a StateLoader for loading ent.Schema's as StateReader's.
type EntLoader struct{}

// LoadState returns a migrate.StateReader that reads the schema from an ent.Schema.
func (l EntLoader) LoadState(ctx context.Context, opts *LoadStateOptions) (migrate.StateReader, error) {
	switch {
	case opts.Dev == nil, len(opts.URLs) != 1:
		return nil, errors.New("schema url and dev database are required")
	case opts.URLs[0].Query().Has("globalid"):
		return nil, errors.New("globalid is not supported by this command. Use 'migrate diff' instead")
	}
	tables, err := l.tables(opts.URLs[0])
	if err != nil {
		return nil, err
	}
	m, err := entschema.NewMigrate(sql.OpenDB(opts.Dev.Name, opts.Dev.DB))
	if err != nil {
		return nil, fmt.Errorf("creating migrate reader: %w", err)
	}
	realm, err := m.StateReader(tables...).ReadState(ctx)
	if err != nil {
		return nil, fmt.Errorf("reading schema state: %w", err)
	}
	if nr, ok := opts.Dev.Driver.(schema.Normalizer); ok {
		if realm, err = nr.NormalizeRealm(ctx, realm); err != nil {
			return nil, err
		}
	}
	return migrate.Realm(realm), nil
}

// MigrateDiff returns the diff between ent.Schema and a directory.
func (l EntLoader) MigrateDiff(ctx context.Context, opts *MigrateDiffOptions) error {
	if !l.needDiff(opts.To) {
		return errors.New("invalid diff call")
	}
	u, err := url.Parse(opts.To[0])
	if err != nil {
		return nil
	}
	tables, err := l.tables(u)
	if err != nil {
		return err
	}
	m, err := entschema.NewMigrate(
		sql.OpenDB(opts.Dev.Name, opts.Dev.DB),
		entschema.WithFormatter(migrate.DefaultFormatter),
		entschema.WithGlobalUniqueID(true),
		entschema.WithDir(opts.Dir),
		entschema.WithMigrationMode(entschema.ModeReplay),
	)
	if err != nil {
		return fmt.Errorf("creating migrate reader: %w", err)
	}
	return m.NamedDiff(ctx, opts.Name, tables...)
}

// needDiff indicates if we need to offload the diffing to Ent in
// case global unique id is enabled in versioned migration mode.
func (EntLoader) needDiff(to []string) bool {
	if len(to) != 1 {
		return false
	}
	u1, err := url.Parse(to[0])
	if err != nil || u1.Scheme != "ent" {
		return false
	}
	gid, _ := strconv.ParseBool(u1.Query().Get("globalid"))
	return gid
}

func (EntLoader) tables(u *url.URL) ([]*entschema.Table, error) {
	abs, err := filepath.Abs(filepath.Join(u.Host, u.Path))
	if err != nil {
		return nil, err
	}
	graph, err := entc.LoadGraph(abs, &gen.Config{})
	if err != nil {
		return nil, fmt.Errorf("loading schema: %w", err)
	}
	return graph.Tables()
}
