// Copyright 2018 CUE Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package cmd

import (
	"cmp"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/spf13/pflag"
	"golang.org/x/text/language"

	"cuelang.org/go/cue"
	"cuelang.org/go/cue/ast"
	"cuelang.org/go/cue/build"
	"cuelang.org/go/cue/errors"
	"cuelang.org/go/cue/load"
	"cuelang.org/go/cue/parser"
	"cuelang.org/go/cue/token"
	"cuelang.org/go/internal/cuedebug"
	"cuelang.org/go/internal/encoding"
	"cuelang.org/go/internal/filetypes"
)

func defaultConfig() (*config, error) {
	reg, err := getCachedRegistry()
	if err != nil {
		return nil, err
	}
	return &config{
		loadCfg: &load.Config{
			ParseFile: func(name string, src interface{}, cfg parser.Config) (*ast.File, error) {
				cuedebug.Init()
				if cuedebug.Flags.ParserTrace {
					cfg = cfg.Apply(parser.Trace)
				}
				return parser.ParseFile(name, src, cfg)
			},
			Registry: reg,
		},
	}, nil
}

func getLang() language.Tag {
	loc := cmp.Or(os.Getenv("LC_ALL"), os.Getenv("LANG"))
	loc, _, _ = strings.Cut(loc, ".")
	return language.Make(loc)
}

func loadFromArgs(args []string, cfg *load.Config) []*build.Instance {
	binst := load.Instances(args, cfg)
	if len(binst) == 0 {
		return nil
	}

	return binst
}

// A buildPlan defines what should be done based on command line
// arguments and flags.
//
// TODO: allow --merge/-m to mix in other packages.
type buildPlan struct {
	cmd   *Command
	insts []*build.Instance

	// instance is a pre-compiled instance, which exists if value files are
	// being processed, which may require a schema to decode.
	instance *instance

	cfg *config

	// If orphanFiles are mixed with CUE files and/or if placement flags are used,
	// the instance is also included in insts.
	importing      bool
	mergeData      bool // do not merge individual data files.
	orphaned       []*decoderInfo
	orphanInstance *build.Instance
	// imported files are files that were orphaned in the build instance, but
	// were placed in the instance by using one the --files, --list or --path
	// flags.
	imported []*ast.File

	expressions []ast.Expr // only evaluate these expressions within results
	schema      ast.Expr   // selects schema in instance for orphaned values

	// orphan placement flags.
	perFile    bool
	useList    bool
	path       []ast.Label
	useContext bool

	// outFile defines the file to output to. Default is CUE stdout.
	outFile *build.File

	encConfig *encoding.Config
}

// instances iterates either over a list of instances, or a list of
// data files. In the latter case, there must be either 0 or 1 other
// instance, with which the data instance may be merged.
func (b *buildPlan) instances() iterator {
	var i iterator
	switch {
	case len(b.orphaned) > 0:
		i = newStreamingIterator(b)
	case len(b.insts) > 0:
		insts, err := buildInstances(b.cmd, b.insts, false)
		i = &instanceIterator{
			inst: b.instance,
			a:    insts,
			e:    err,
			i:    -1,
		}
	case b.instance != nil:
		i = &instanceIterator{
			a: []*instance{b.instance},
			i: -1,
		}
		b.instance = nil
	default:
		// No instances; return an iterator with zero values.
		// This can happen when the input is an empty jsonl file, for example.
		// Zero iteration may not make much sense in some scenarios like export,
		// but an empty jsonl file is valid, so doing nothing seems reasonable.
		i = &instanceIterator{}
	}
	if len(b.expressions) > 0 {
		return &expressionIter{
			iter: i,
			expr: b.expressions,
			i:    len(b.expressions),
		}
	}
	return i
}

type iterator interface {
	scan() bool
	value() cue.Value
	file() *ast.File // may return nil
	err() error
	close()
	id() string // may return ""
}

type instance struct {
	id  string
	err error
	val cue.Value
}

func (i *instance) Value() cue.Value { return i.val }

type instanceIterator struct {
	inst *instance
	a    []*instance
	i    int
	e    error
}

func (i *instanceIterator) scan() bool {
	i.i++
	return i.i < len(i.a) && i.e == nil
}

func (i *instanceIterator) close()     {}
func (i *instanceIterator) err() error { return i.e }
func (i *instanceIterator) value() cue.Value {
	v := i.a[i.i].Value()
	if i.inst != nil {
		v = v.Unify(i.inst.Value())
	}
	return v
}
func (i *instanceIterator) file() *ast.File { return nil }
func (i *instanceIterator) id() string {
	if i.i > len(i.a) {
		return ""
	}
	return i.a[i.i].id
}

type streamingIterator struct {
	b   *buildPlan
	cfg *encoding.Config
	a   []*decoderInfo
	dec *encoding.Decoder
	v   cue.Value
	f   *ast.File
	e   error
}

func newStreamingIterator(b *buildPlan) *streamingIterator {
	i := &streamingIterator{
		cfg: b.encConfig,
		a:   b.orphaned,
		b:   b,
	}
	return i
}

func (i *streamingIterator) file() *ast.File  { return i.f }
func (i *streamingIterator) value() cue.Value { return i.v }
func (i *streamingIterator) id() string       { return "" }

func (i *streamingIterator) scan() bool {
	if i.e != nil {
		return false
	}

	// advance to next value
	if i.dec != nil && !i.dec.Done() {
		i.dec.Next()
		if i.e = i.dec.Err(); i.e != nil {
			return false
		}
	}

	// advance to next stream if necessary
	for i.dec == nil || i.dec.Done() {
		if i.dec != nil {
			i.dec.Close()
			i.dec = nil
		}
		if len(i.a) == 0 {
			return false
		}

		i.dec = i.a[0].dec(i.b)
		if i.e = i.dec.Err(); i.e != nil {
			return false
		}
		i.a = i.a[1:]
	}

	// compose value
	i.f = i.dec.File()
	v := i.b.cmd.ctx.BuildFile(i.f)
	if err := v.Err(); err != nil {
		i.e = err
		return false
	}
	i.v = v
	if schema := i.b.encConfig.Schema; schema.Exists() {
		i.v = i.v.Unify(schema) // TODO(required fields): don't merge in schema
		i.e = i.v.Err()
		if i.e != nil {
			if err := i.v.Validate(); err != nil {
				// Validate should always be non-nil, but just in case.
				i.e = err
			}
		}
		i.f = nil
	}
	return i.e == nil
}

func (i *streamingIterator) close() {
	if i.dec != nil {
		i.dec.Close()
		i.dec = nil
	}
}

func (i *streamingIterator) err() error {
	if i.dec != nil {
		if err := i.dec.Err(); err != nil {
			return err
		}
	}
	return i.e
}

type expressionIter struct {
	iter iterator
	expr []ast.Expr
	i    int
}

func (i *expressionIter) err() error { return i.iter.err() }
func (i *expressionIter) close()     { i.iter.close() }
func (i *expressionIter) id() string { return i.iter.id() }

func (i *expressionIter) scan() bool {
	i.i++
	if i.i < len(i.expr) {
		return true
	}
	if !i.iter.scan() {
		return false
	}
	i.i = 0
	return true
}

func (i *expressionIter) file() *ast.File { return nil }

func (i *expressionIter) value() cue.Value {
	if len(i.expr) == 0 {
		return i.iter.value()
	}
	v := i.iter.value()
	return v.Context().BuildExpr(i.expr[i.i],
		cue.Scope(v),
		cue.InferBuiltins(true),
		cue.ImportPath(i.iter.id()),
	)
}

type config struct {
	mode filetypes.Mode

	fileFilter     string
	reFile         *regexp.Regexp
	encoding       build.Encoding
	interpretation build.Interpretation

	overrideDefault bool

	noMerge bool // do not merge individual data files.

	loadCfg *load.Config
}

func newBuildPlan(cmd *Command, cfg *config) (p *buildPlan, err error) {
	var defCfg *config
	if cfg == nil || cfg.loadCfg == nil {
		var err error
		defCfg, err = defaultConfig()
		if err != nil {
			return nil, err
		}
	}
	if cfg == nil {
		cfg = defCfg
	}
	if cfg.loadCfg == nil {
		cfg.loadCfg = defCfg.loadCfg
	}
	cfg.loadCfg.Stdin = cmd.InOrStdin()

	p = &buildPlan{
		cfg:       cfg,
		cmd:       cmd,
		importing: cfg.loadCfg.DataFiles,
	}

	if err := p.parseFlags(); err != nil {
		return nil, err
	}
	re, err := regexp.Compile(p.cfg.fileFilter)
	if err != nil {
		return nil, err
	}
	cfg.reFile = re

	setTags(cfg.loadCfg, cmd.Flags())

	return p, nil
}

func (p *buildPlan) matchFile(file string) bool {
	return p.cfg.reFile.MatchString(file)
}

func setTags(cfg *load.Config, flags *pflag.FlagSet) {
	tags, _ := flags.GetStringArray(string(flagInject))
	cfg.Tags = append(cfg.Tags, tags...)
	if b, _ := flags.GetBool(string(flagInjectVars)); b {
		cfg.TagVars = load.DefaultTagVars()
	}
}

type decoderInfo struct {
	file *build.File
	d    *encoding.Decoder // may be nil if delayed
}

func (d *decoderInfo) dec(b *buildPlan) *encoding.Decoder {
	if d.d == nil {
		d.d = encoding.NewDecoder(b.cmd.ctx, d.file, b.encConfig)
	}
	return d.d
}

// getDecoders takes the orphaned files of the given instance and splits them in
// schemas and values, saving the build.File and encoding.Decoder in the
// returned slices. It is up to the caller to Close any of the decoders that are
// returned.
func (p *buildPlan) getDecoders(b *build.Instance) (schemas, values []*decoderInfo, err error) {
	files := b.OrphanedFiles
	if p.cfg.overrideDefault {
		files = append(files, b.UnknownFiles...)
	}
	for _, f := range files {
		if !b.User && !p.matchFile(f.Filename) {
			continue
		}
		if p.cfg.overrideDefault {
			f.Encoding = p.cfg.encoding
			f.Interpretation = p.cfg.interpretation
		}
		switch f.Encoding {
		case build.Protobuf, build.YAML, build.TOML, build.XML, build.JSON, build.JSONL,
			build.Text, build.Binary:
			if f.Interpretation == build.ProtobufJSON {
				// Need a schema.
				values = append(values, &decoderInfo{f, nil})
				continue
			}
		case build.TextProto:
			if p.importing {
				return schemas, values, errors.Newf(token.NoPos,
					"cannot import textproto files")
			}
			// Needs to be decoded after any schema.
			values = append(values, &decoderInfo{f, nil})
			continue
		default:
			return schemas, values, errors.Newf(token.NoPos,
				"unsupported encoding %q", f.Encoding)
		}

		// We add the module root to the path if there is a module defined.
		c := *p.encConfig
		if b.Module != "" {
			c.ProtoPath = append(c.ProtoPath, b.Root)
		}
		d := encoding.NewDecoder(p.cmd.ctx, f, &c)

		fi, err := filetypes.FromFile(f, p.cfg.mode)
		if err != nil {
			return schemas, values, err
		}
		switch {
		// case !fi.Schema: // TODO: value/schema/auto
		// 	values = append(values, d)
		case fi.Form != build.Schema && fi.Form != build.Final:
			values = append(values, &decoderInfo{f, d})

		case f.Interpretation != build.Auto:
			schemas = append(schemas, &decoderInfo{f, d})

		case d.Interpretation() == "":
			values = append(values, &decoderInfo{f, d})

		default:
			schemas = append(schemas, &decoderInfo{f, d})
		}
	}
	return schemas, values, nil
}

// importFiles imports orphan files for existing instances. Note that during
// import, both schemas and non-schemas are placed (TODO: should we allow schema
// mode here as well? It seems that the existing package should have enough
// typing to allow for schemas).
//
// It is a separate call to allow closing decoders between processing each
// package.
func (p *buildPlan) importFiles(b *build.Instance) error {
	// TODO: assume textproto is imported at top-level or just ignore them.

	schemas, values, err := p.getDecoders(b)
	if err != nil {
		return err
	}
	return p.placeOrphans(b, append(schemas, values...))
}

func parseArgs(cmd *Command, args []string, cfg *config) (p *buildPlan, err error) {
	p, err = newBuildPlan(cmd, cfg)
	if err != nil {
		return nil, err
	}

	builds := loadFromArgs(args, cfg.loadCfg)
	if builds == nil {
		return nil, errors.Newf(token.NoPos, "invalid args")
	}

	if err := p.parsePlacementFlags(); err != nil {
		return nil, err
	}

	for _, b := range builds {
		if b.Err != nil {
			return nil, suggestModCommand(b.Err)
		}
		switch {
		case !b.User:
			if p.importing {
				if err := p.importFiles(b); err != nil {
					return nil, err
				}
			}
			p.insts = append(p.insts, b)

		case p.orphanInstance != nil:
			return nil, errors.Newf(token.NoPos,
				"builds contain two file packages")

		default:
			p.orphanInstance = b
		}
	}

	if len(p.insts) == 0 && flagGlob.String(p.cmd) != "" {
		return nil, errors.Newf(token.NoPos,
			"use of -n/--name flag without a directory")
	}

	if b := p.orphanInstance; b != nil {
		schemas, values, err := p.getDecoders(b)
		if err != nil {
			return nil, err
		}

		if values == nil {
			values, schemas = schemas, values
		}

		for _, di := range schemas {
			d := di.dec(p)
			for ; !d.Done(); d.Next() {
				if err := b.AddSyntax(d.File()); err != nil {
					return nil, err
				}
			}
			if err := d.Err(); err != nil {
				return nil, err
			}
			d.Close()
		}

		if len(p.insts) > 1 && p.schema != nil {
			return nil, errors.Newf(token.NoPos,
				"cannot use --schema/-d with flag more than one schema")
		}

		var schema *build.Instance
		switch n := len(p.insts); n {
		default:
			return nil, errors.Newf(token.NoPos,
				"too many packages defined (%d) in combination with files", n)
		case 1:
			if len(schemas) > 0 {
				return nil, errors.Newf(token.NoPos,
					"cannot combine packages with individual schema files")
			}
			schema = p.insts[0]
			p.insts = nil

		case 0:
			bb := *b
			schema = &bb
			b.BuildFiles = nil
			b.Files = nil
		}

		if schema != nil && len(schema.Files) > 0 {
			// TODO: ignore errors here for now until reporting of concreteness
			// of errors is correct.
			// See https://github.com/cue-lang/cue/issues/1483.
			insts, err := buildInstances(
				p.cmd,
				[]*build.Instance{schema},
				true)
			if err != nil {
				return nil, err
			}
			inst := insts[0]
			if err := inst.err; err != nil {
				return nil, err
			}
			p.instance = inst
			p.encConfig.Schema = inst.Value()
			if p.schema != nil {
				v := cmd.ctx.BuildExpr(p.schema,
					cue.InferBuiltins(true),
					cue.Scope(inst.Value()))
				// Note that we don't check v.Err as we don't care about
				// incomplete errors.
				if err := v.Validate(); err != nil {
					return nil, err
				}
				p.encConfig.Schema = v
			}
		} else if p.schema != nil {
			return nil, errors.Newf(token.NoPos,
				"-d/--schema flag specified without a schema")
		}

		switch {
		default:
			fallthrough

		case p.schema != nil:
			p.orphaned = values

		case p.mergeData, p.usePlacement(), p.importing:
			if err = p.placeOrphans(b, values); err != nil {
				return nil, err
			}

		}

		if len(b.Files) > 0 {
			p.insts = append(p.insts, b)
		}
	}

	if len(p.expressions) > 1 {
		p.encConfig.Stream = true
	}
	return p, nil
}

func (b *buildPlan) parseFlags() (err error) {
	b.mergeData = !b.cfg.noMerge && flagMerge.Bool(b.cmd)

	if flagStrict.IsSet(b.cmd) {
		return fmt.Errorf(`--strict is deprecated; use "jsonschema+strict:" as shown in "cue help filetypes"`)
	}
	b.encConfig = &encoding.Config{
		Mode:      b.cfg.mode,
		Stdin:     b.cmd.InOrStdin(),
		Stdout:    b.cmd.OutOrStdout(),
		ProtoPath: flagProtoPath.StringArray(b.cmd),
		AllErrors: flagAllErrors.Bool(b.cmd),
		PkgName:   flagPackage.String(b.cmd),
	}

	// For commands with an output mode, like `cue export` or `cue def`.
	if b.cfg.mode != filetypes.Input {
		out := flagOut.String(b.cmd)
		outFile := flagOutFile.String(b.cmd)

		if strings.Contains(out, ":") && strings.Contains(outFile, ":") {
			return errors.Newf(token.NoPos,
				"cannot specify qualifier in both --out and --outfile")
		}
		if outFile == "" {
			outFile = "-"
		}
		if out != "" {
			outFile = out + ":" + outFile
		}
		b.outFile, err = filetypes.ParseFile(outFile, b.cfg.mode)
		if err != nil {
			return err
		}

		for _, e := range flagExpression.StringArray(b.cmd) {
			expr, err := parser.ParseExpr("--expression", e)
			if err != nil {
				return err
			}
			b.expressions = append(b.expressions, expr)
		}
		b.encConfig.Force = flagForce.Bool(b.cmd)
	}

	if s := flagSchema.String(b.cmd); s != "" {
		b.schema, err = parser.ParseExpr("--schema", s)
		if err != nil {
			return err
		}
	}
	if s := flagGlob.String(b.cmd); s != "" {
		// Set a default file filter to only include json and yaml files
		b.cfg.fileFilter = s
	}
	// These flags exist only in specific output modes.
	switch b.cfg.mode {
	case filetypes.Export:
		b.encConfig.EscapeHTML = flagEscape.Bool(b.cmd)
	case filetypes.Def:
		b.encConfig.InlineImports = flagInlineImports.Bool(b.cmd)
	}
	return nil
}

func buildInstances(cmd *Command, binst []*build.Instance, ignoreErrors bool) ([]*instance, error) {
	// TODO:
	// If there are no files and User is true, then use those?
	// Always use all files in user mode?
	instances, err := cmd.ctx.BuildInstances(binst)
	if err != nil {
		return nil, err
	}

	insts := make([]*instance, len(instances))
	for i, v := range instances {
		insts[i] = &instance{
			id:  binst[i].ID(),
			err: binst[i].Err,
			val: v,
		}
	}

	// TODO: remove ignoreErrors flag and always return here, leaving it up to
	// clients to check for errors down the road.
	if ignoreErrors || flagIgnore.Bool(cmd) {
		return insts, nil
	}

	for _, inst := range instances {
		// TODO: consider merging errors of multiple files, but ensure
		// duplicates are removed.
		err := inst.Validate()
		if err != nil {
			if flagIgnore.Bool(cmd) {
				printError(cmd, err)
			} else {
				return nil, err
			}
		}
	}
	return insts, nil
}

func buildToolInstances(ctx *cue.Context, binst []*build.Instance) ([]*cue.Instance, error) {
	// Reuse the same context, if there is one, so that the @embed interpreter can be used.
	// Note that ctx may be nil when we do `cue help cmd`.
	r := new(cue.Runtime)
	if ctx != nil {
		r = (*cue.Runtime)(ctx)
	}
	instances, _ := r.BuildInstances(binst)
	for _, inst := range instances {
		if inst.Err != nil {
			return nil, inst.Err
		}
	}

	// TODO check errors after the fact in case of ignore.
	for _, inst := range instances {
		if err := inst.Value().Validate(); err != nil {
			return nil, err
		}
	}
	return instances, nil
}

func buildTools(cmd *Command, args []string) (*cue.Instance, error) {
	cfg, err := defaultConfig()
	if err != nil {
		return nil, err
	}
	loadCfg := *cfg.loadCfg
	loadCfg.Tools = true
	setTags(&loadCfg, cmd.cmdCmd.Flags())

	binst := loadFromArgs(args, &loadCfg)
	if len(binst) == 0 {
		return nil, nil
	}
	included := map[string]bool{}

	ti := binst[0].Context().NewInstance(binst[0].Root, nil)
	// For @embed to also work in _tool.cue files, we must pass the module info along.
	ti.Module = binst[0].Module
	ti.Root = binst[0].Root

	for _, inst := range binst {
		k := 0
		for _, f := range inst.Files {
			if strings.HasSuffix(f.Filename, "_tool.cue") {
				if !included[f.Filename] {
					_ = ti.AddSyntax(f)
					included[f.Filename] = true
				}
				continue
			}
			inst.Files[k] = f
			k++
		}
		inst.Files = inst.Files[:k]
	}

	insts, err := buildToolInstances(cmd.ctx, binst)
	if err != nil {
		return nil, err
	}

	inst := insts[0]
	if len(insts) > 1 {
		inst = cue.Merge(insts...)
	}

	ctx := inst.Value().Context()
	for _, b := range binst {
		for _, i := range b.Imports {
			val := ctx.BuildInstance(i)
			if err := val.Err(); err != nil {
				return nil, err
			}
		}
	}

	// Set path equal to the package from which it is loading.
	ti.ImportPath = binst[0].ImportPath

	inst = inst.Build(ti)
	return inst, inst.Err
}

func shortFile(root string, f *build.File) string {
	dir, _ := filepath.Rel(root, f.Filename)
	if dir == "" {
		return f.Filename
	}
	if !filepath.IsAbs(dir) {
		dir = "." + string(filepath.Separator) + dir
	}
	return dir
}
